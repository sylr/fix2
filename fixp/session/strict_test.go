package session

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/sofh"
	"github.com/sylr/fix2/transport/mem"
)

// The S4 rows of docs/conformance.md: FIXP, SOFH and SBE followed strictly
// by default, each departure a toleration, each recommendation an option.

// rawFrame frames m, its header's version set to version, n zero bytes
// after it.
func rawFrame(t testing.TB, m encoder, version uint16, n int) []byte {
	t.Helper()
	msg, err := m.AppendTo(nil)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(msg[6:], version)
	msg = append(msg, make([]byte, n)...)
	f, err := sofh.AppendFrame(nil, sofh.SBE10LittleEndian, msg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// closedSilently checks the connection ends without a frame.
func (r *raw) closedSilently() {
	r.t.Helper()
	r.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, msg, err := r.r.Next()
	switch {
	case err == nil:
		h, _, _ := codec.ReadHeader(msg)
		r.t.Fatalf("template %d, want the connection closed", h.TemplateId)
	case errors.Is(err, os.ErrDeadlineExceeded):
		r.t.Fatal("connection not closed")
	}
}

// syncBuffer is a log's buffer, written by the session's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// KeepaliveLeniency is at least 1, finite, and its product with
// MaxKeepalive a time.Duration, on either role.
func TestKeepaliveLeniencyConfig(t *testing.T) {
	for _, tc := range []struct {
		leniency float64
		max      time.Duration
		ok       bool
	}{
		{0, 0, true}, {1, 0, true}, {30, 0, true}, {1.5e8, 0, true}, {2e8, time.Second, true},
		{0.5, 0, false}, {-1, 0, false}, {math.NaN(), 0, false}, {math.Inf(1), 0, false}, {math.Inf(-1), 0, false},
		{2e8, 0, false}, {1e30, 0, false},
	} {
		cfg := Config{Handler: BaseHandler{}, KeepaliveLeniency: tc.leniency, MaxKeepalive: tc.max}
		_, cerr := ClientConfig{Config: cfg, Store: memstore.New()}.resolve()
		_, serr := ServerConfig{Config: cfg, Store: memstore.New(), Authenticate: authenticate}.resolve()
		for _, err := range []error{cerr, serr} {
			var ce *ConfigError
			if tc.ok && err != nil || !tc.ok && (!errors.As(err, &ce) || ce.Field != "KeepaliveLeniency") {
				t.Errorf("KeepaliveLeniency %v, MaxKeepalive %v: %v", tc.leniency, tc.max, err)
			}
		}
	}
	if _, err := (ClientConfig{Config: Config{Handler: BaseHandler{}, MinKeepalive: time.Second, MaxKeepalive: time.Millisecond}, Store: memstore.New()}).resolve(); err == nil {
		t.Error("a client's MaxKeepalive below its MinKeepalive accepted")
	}
}

// A peer silent beyond its keepalive interval is cut off (§4.5), unless
// ToleratePeerWithoutKeepalives.
func TestToleratePeerWithoutKeepalives(t *testing.T) {
	for _, tolerate := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "tolerated"}[tolerate], func(t *testing.T) {
			r, h, _, _ := rawClientWith(t, fixp.Unsequenced, fixp.Unsequenced, 20, func(c *ServerConfig) { c.ToleratePeerWithoutKeepalives = tolerate })
			if !tolerate {
				r.terminated(fixp.TerminationUnspecifiedError, "keepalive interval lapsed")
				return
			}
			time.Sleep(200 * time.Millisecond) // ten intervals
			r.send(appMsg{1, "late"})
			if e := h.expect(t, "message"); e.body != "late" {
				t.Fatalf("message %v", e)
			}
		})
	}
}

// Bytes after a session message within its frame make it malformed,
// unless TolerateTrailingBytes and its version is not above the schema's;
// a newer version without them is read as far as it is known.
func TestTrailingBytes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tolerate bool
		version  uint16
		pad      int
		ok       bool
	}{
		{"padded", false, 0, 4, false},
		{"padded, tolerated", true, 0, 4, true},
		{"newer, padded, tolerated", true, 1, 4, false},
		{"newer", false, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, h, _, _ := rawClientWith(t, fixp.Unsequenced, fixp.Unsequenced, 1000, func(c *ServerConfig) { c.TolerateTrailingBytes = tc.tolerate })
			r.sendBytes(rawFrame(t, &codec.UnsequencedHeartbeat{}, tc.version, tc.pad))
			if !tc.ok {
				r.terminated(fixp.TerminationUnspecifiedError, "UnsequencedHeartbeat: 4 bytes after the message")
				return
			}
			r.send(appMsg{1, "after"})
			if e := h.expect(t, "message"); e.body != "after" {
				t.Fatalf("message %v", e)
			}
		})
	}
	// A client's handshake: a padded NegotiationResponse is malformed, the
	// negotiation tried again on a new connection; tolerated, it goes on.
	for _, tolerate := range []bool{false, true} {
		t.Run(map[bool]string{false: "client", true: "client, tolerated"}[tolerate], func(t *testing.T) {
			_, _, l, _ := clientOfRawWith(t, fixp.Unsequenced, func(c *ClientConfig) { c.TolerateTrailingBytes = tolerate })
			r := acceptRaw(t, l)
			var n codec.Negotiate
			if _, err := n.Decode(r.readTemplate(codec.NegotiateTemplateID)); err != nil {
				t.Fatal(err)
			}
			r.sendBytes(rawFrame(t, &codec.NegotiationResponse{SessionId: n.SessionId, RequestTimestamp: n.Timestamp, ServerFlow: codec.FlowTypeUnsequenced}, 0, 6))
			if tolerate {
				r.readEstablish()
				return
			}
			r.closed()
			acceptRaw(t, l).readTemplate(codec.NegotiateTemplateID)
		})
	}
}

// A session message with a null in a required field, an unknown
// enumeration value, or a newer version's data the decoder cannot bound,
// is malformed: the connection terminates (SBE §4.15, FIXP §4.4). A
// Terminate whose code is unknown is answered UnspecifiedError, never
// with that code.
func TestMalformedSessionMessages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		send   func(r *raw)
		reason string
	}{
		{"Terminate of an unknown code", func(r *raw) {
			r.send(&codec.Terminate{SessionId: r.id, Code: 9})
		}, "Terminate: Code TerminationCode(9) not a valid value"},
		{"Terminate of a newer version", func(r *raw) {
			r.sendBytes(rawFrame(r.t, &codec.Terminate{SessionId: r.id}, 1, 0))
		}, "Terminate: " + codec.ErrNewer.Error()},
		{"Sequence null", func(r *raw) {
			r.send(&codec.Sequence{NextSeqNo: math.MaxUint64})
		}, "Sequence: NextSeqNo null"},
		{"RetransmitRequest's Timestamp null", func(r *raw) {
			r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: math.MaxUint64, FromSeqNo: 1, Count: 1})
		}, "RetransmitRequest: Timestamp null"},
		{"RetransmitRequest's Count null", func(r *raw) {
			r.send(&codec.RetransmitRequest{SessionId: r.id, Timestamp: 1, FromSeqNo: 1, Count: math.MaxUint32})
		}, "RetransmitRequest: Count null"},
		{"Applied's FromSeqNo null", func(r *raw) {
			r.send(&codec.Applied{FromSeqNo: math.MaxUint64, Count: 1})
		}, "Applied: FromSeqNo null"},
		{"MessageTemplate's EncodingType null", func(r *raw) {
			r.send(&codec.MessageTemplate{EncodingType: math.MaxUint32})
		}, "MessageTemplate: EncodingType null"},
		{"Establish's KeepaliveInterval null", func(r *raw) {
			r.send(&codec.Establish{SessionId: r.id, Timestamp: nanos(time.Now()), KeepaliveInterval: math.MaxUint32})
		}, "Establish: KeepaliveInterval null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _, _ := rawClient(t, fixp.Idempotent, fixp.Recoverable, 1000)
			tc.send(r)
			r.terminated(fixp.TerminationUnspecifiedError, tc.reason)
		})
	}

	// In the handshake, a server closes on a malformed request, unanswered.
	t.Run("server handshake", func(t *testing.T) {
		l := mem.Listen("server")
		_, cancel, done := newServer(t, l, memstore.New(), newRecorder(), fixp.Unsequenced, nil)
		defer func() { cancel(); <-done }()
		r := dialRaw(t, l)
		r.send(&codec.Negotiate{SessionId: r.id, Timestamp: math.MaxUint64, ClientFlow: codec.FlowTypeUnsequenced, Credentials: []byte("user:alice")})
		r.closedSilently()
		r = dialRaw(t, l)
		r.negotiate(fixp.Unsequenced, "user:alice")
		r.send(&codec.Establish{SessionId: r.id, Timestamp: nanos(time.Now()), KeepaliveInterval: math.MaxUint32, Credentials: []byte("user:alice")})
		r.closedSilently()
	})
	// A client tries again on a malformed answer: a NegotiationReject of
	// an unknown code is no rejection to stop at, an EstablishmentAck whose
	// keepalive interval is null no establishment.
	t.Run("client handshake", func(t *testing.T) {
		_, h, l, ran := clientOfRawWith(t, fixp.Unsequenced, nil)
		r := acceptRaw(t, l)
		var n codec.Negotiate
		if _, err := n.Decode(r.readTemplate(codec.NegotiateTemplateID)); err != nil {
			t.Fatal(err)
		}
		r.send(&codec.NegotiationReject{SessionId: n.SessionId, RequestTimestamp: n.Timestamp, Code: 9})
		r.closed()
		r = acceptRaw(t, l)
		r.answerNegotiate(fixp.Unsequenced)
		e := r.readEstablish()
		r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: math.MaxUint32})
		r.closed()
		r = acceptRaw(t, l)
		e = r.readEstablish()
		r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000})
		h.expect(t, "established")
		select {
		case err := <-ran:
			t.Fatalf("Run returned: %v", err)
		default:
		}
	})
}

// NextSeqNo (§4.3.1, §4.3.2, §4.8): a client's Establish carries it on
// re-establishment of a Recoverable flow only; an EstablishmentAck for a
// Recoverable server flow only (TestExample_7_1_and_7_2_Handshakes). One
// received for an Idempotent flow is accepted and numbers nothing: the
// flow starts with a Sequence.
func TestNextSeqNo(t *testing.T) {
	for _, flow := range []fixp.FlowType{fixp.Recoverable, fixp.Idempotent} {
		t.Run("client "+flow.String(), func(t *testing.T) {
			c, h, l := clientOfRaw(t, flow)
			r := acceptRaw(t, l)
			r.answerNegotiate(fixp.Unsequenced)
			e := r.readEstablish()
			if e.NextSeqNo.Valid {
				t.Fatalf("first Establish with NextSeqNo %d", e.NextSeqNo.Value)
			}
			r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000})
			h.expect(t, "established")
			if _, err := c.Session().Send(bg, appMsg{1, "1"}); err != nil {
				t.Fatal(err)
			}
			if got := r.readApps(1); strings.Join(got, ",") != "seq=1,1" {
				t.Fatalf("first message %q", got)
			}
			r.c.Close()
			h.until(t, "unbound")
			r = acceptRaw(t, l)
			e = r.readEstablish()
			want := codec.Optional[uint64]{}
			if flow == fixp.Recoverable {
				want = codec.Some(uint64(2))
			}
			if e.NextSeqNo != want {
				t.Fatalf("Establish again with NextSeqNo %+v, want %+v", e.NextSeqNo, want)
			}
		})
	}
	t.Run("server, an Idempotent client's", func(t *testing.T) {
		l := mem.Listen("server")
		h := newRecorder()
		_, cancel, done := newServer(t, l, memstore.New(), h, fixp.Recoverable, nil)
		defer func() { cancel(); <-done }()
		r := dialRaw(t, l)
		r.negotiate(fixp.Idempotent, "user:alice")
		r.establish(1000, codec.Some(uint64(1)), "user:alice")
		h.expect(t, "established")
		r.send(appMsg{1, "1"})
		r.terminated(fixp.TerminationUnspecifiedError, "application message before a Sequence")
	})
	t.Run("client, an Idempotent server's", func(t *testing.T) {
		_, h, l := clientOfRaw(t, fixp.Unsequenced)
		r := acceptRaw(t, l)
		r.answerNegotiate(fixp.Idempotent)
		e := r.readEstablish()
		r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000, NextSeqNo: codec.Some(uint64(1))})
		h.expect(t, "established")
		r.send(appMsg{1, "1"})
		r.terminated(fixp.TerminationUnspecifiedError, "application message before a Sequence")
	})
}

// A client bounds the server's keepalive interval (MinKeepalive,
// MaxKeepalive): outside, it terminates and Run returns an error.
func TestClientBoundsServerKeepalive(t *testing.T) {
	for _, tc := range []struct {
		ka     uint32
		reason string
	}{
		{50, "keepalive interval 50ms, not between 100ms and 1s"},
		{5000, "keepalive interval 5s, not between 100ms and 1s"},
		{500, ""},
	} {
		t.Run((time.Duration(tc.ka) * time.Millisecond).String(), func(t *testing.T) {
			_, h, l, ran := clientOfRawWith(t, fixp.Unsequenced, func(c *ClientConfig) {
				c.MinKeepalive, c.MaxKeepalive = 100*time.Millisecond, time.Second
			})
			r := acceptRaw(t, l)
			r.answerNegotiate(fixp.Unsequenced)
			e := r.readEstablish()
			r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: tc.ka})
			if tc.reason == "" {
				h.expect(t, "established")
				return
			}
			r.terminated(fixp.TerminationUnspecifiedError, tc.reason)
			select {
			case err := <-ran:
				if err == nil || !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run goes on")
			}
		})
	}
}

// Applied for a flow not Idempotent, or beyond what was sent, is no
// protocol violation (§4.8.1): delivered, a warning logged.
func TestAppliedUnexpected(t *testing.T) {
	for _, tc := range []struct {
		serverFlow fixp.FlowType
		warning    string
	}{
		{fixp.Recoverable, "Applied of a flow not Idempotent"},
		{fixp.Idempotent, "Applied beyond what was sent"},
	} {
		t.Run(tc.serverFlow.String(), func(t *testing.T) {
			var log syncBuffer
			r, h, _, _ := rawClientWith(t, fixp.Idempotent, tc.serverFlow, 1000, func(c *ServerConfig) {
				c.Logger = slog.New(slog.NewTextHandler(&log, nil))
			})
			r.send(&codec.Applied{FromSeqNo: 1, Count: 1})
			if e := h.expect(t, "applied"); e.seq != 1 || e.body != "1" {
				t.Fatalf("event %v", e)
			}
			if !strings.Contains(log.String(), tc.warning) {
				t.Fatalf("no warning %q in %q", tc.warning, log.String())
			}
			h.quiet(t, 20*time.Millisecond)
		})
	}
}

// A MessageTemplate in the handshake is ignored (§3.6), the handshake's
// deadline unchanged, by a server and a client.
func TestMessageTemplateInHandshake(t *testing.T) {
	mt := &codec.MessageTemplate{EncodingType: uint32(sofh.SBE10LittleEndian), Version: []byte("1.0"), Template: []byte("<messageSchema/>")}
	t.Run("server", func(t *testing.T) {
		l := mem.Listen("server")
		h := newRecorder()
		_, cancel, done := newServer(t, l, memstore.New(), h, fixp.Unsequenced, func(c *ServerConfig) { c.FirstMessageTimeout = 100 * time.Millisecond })
		defer func() { cancel(); <-done }()
		r := dialRaw(t, l)
		r.send(mt)
		var resp codec.NegotiationResponse
		if _, err := resp.Decode(r.negotiate(fixp.Unsequenced, "user:alice")); err != nil {
			t.Fatal(err)
		}
		r.send(mt)
		var ack codec.EstablishmentAck
		if _, err := ack.Decode(r.establish(1000, codec.Optional[uint64]{}, "user:alice")); err != nil {
			t.Fatal(err)
		}
		h.expect(t, "established")

		// Templates alone: the connection closes at its deadline.
		r = dialRaw(t, l)
		start := time.Now()
		go func() {
			for time.Since(start) < time.Second {
				f, _ := frame(mt)
				if _, err := r.c.Write(f); err != nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		r.closed()
		if d := time.Since(start); d > 600*time.Millisecond {
			t.Fatalf("closed after %v: the templates extended the deadline", d)
		}
	})
	t.Run("client", func(t *testing.T) {
		_, h, l := clientOfRaw(t, fixp.Unsequenced)
		r := acceptRaw(t, l)
		var n codec.Negotiate
		if _, err := n.Decode(r.readTemplate(codec.NegotiateTemplateID)); err != nil {
			t.Fatal(err)
		}
		r.send(mt)
		r.send(&codec.NegotiationResponse{SessionId: n.SessionId, RequestTimestamp: n.Timestamp, ServerFlow: codec.FlowTypeUnsequenced})
		e := r.readEstablish()
		r.send(mt)
		r.send(&codec.EstablishmentAck{SessionId: e.SessionId, RequestTimestamp: e.Timestamp, KeepaliveInterval: 1000})
		h.expect(t, "established")
	})
	t.Run("client deadline", func(t *testing.T) {
		_, _, l := clientOfRaw(t, fixp.Unsequenced) // HandshakeTimeout 100ms
		r := acceptRaw(t, l)
		r.readTemplate(codec.NegotiateTemplateID)
		start := time.Now()
		go func() {
			for time.Since(start) < time.Second {
				f, _ := frame(mt)
				if _, err := r.c.Write(f); err != nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		r.closed()
		if d := time.Since(start); d > 600*time.Millisecond {
			t.Fatalf("closed after %v: the templates extended the deadline", d)
		}
	})
}
