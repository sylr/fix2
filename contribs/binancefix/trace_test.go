package binancefix

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/transport"
)

// tracer records the messages Config.Trace receives.
type tracer struct {
	mu   sync.Mutex
	msgs []TraceMessage
}

// trace records m, its message copied: it is valid during the call only.
func (r *tracer) trace(m TraceMessage) {
	if e, ok := m.Message.(interface {
		AppendWithHeader([]byte, MessageHeader) ([]byte, error)
	}); ok {
		b, err := e.AppendWithHeader(nil, m.Header)
		if err != nil {
			panic(err)
		}
		if m.Message, err = decoders[m.Header.TemplateId](m.Header, b[fixsbe.HeaderSize:]); err != nil {
			panic(err)
		}
	}
	m.Body = bytes.Clone(m.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
}

// wait waits for a message of template tid, of direction out, and returns
// it.
func (r *tracer) wait(t *testing.T, out bool, tid uint16) TraceMessage {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		r.mu.Lock()
		for _, m := range r.msgs {
			if m.Out == out && m.Header.TemplateId == tid {
				r.mu.Unlock()
				return m
			}
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no traced message of template %d (out %v)", tid, out)
		}
		time.Sleep(time.Millisecond)
	}
}

// Config.Trace sees every message, both ways, decoded: those the Handler
// never sees (the Logon, Heartbeats, TestRequests) included, the Logon's
// credentials redacted though they go on the wire.
func TestTrace(t *testing.T) {
	r := &tracer{}
	c, _, p, h, _ := mdSession(t, time.Hour, func(c *Config) { c.Trace = r.trace })
	logon := r.wait(t, true, fixsbe.LogonTemplateID)
	if l, ok := logon.Message.(*Logon); !ok || string(l.Username) != Redacted || string(l.RawData) != Redacted ||
		string(l.SenderCompId) != c.cfg.SenderCompID || logon.Epoch != 1 || logon.Header.SeqNum != 1 {
		t.Errorf("Logon traced %+v", logon)
	}
	if m := r.wait(t, false, fixsbe.LogonAckTemplateID); m.Out || m.Message.(*LogonAck).HeartBtInt == 0 {
		t.Errorf("LogonAck traced %+v", m)
	}
	if _, err := c.Subscribe(trades("T", "BTCUSDT")); err != nil {
		t.Fatal(err)
	}
	p.expectMDR(t)
	if m := r.wait(t, true, fixsbe.MarketDataRequestTemplateID); string(m.Message.(*MarketDataRequest).MDReqID) != "T" || m.Header.SeqNum != 2 {
		t.Errorf("MarketDataRequest traced %+v", m)
	}
	p.send(t, &fixsbe.TestRequest{TestReqID: []byte("tr")})
	p.expect(t, fixsbe.HeartbeatTemplateID)
	r.wait(t, false, fixsbe.TestRequestTemplateID)
	if m := r.wait(t, true, fixsbe.HeartbeatTemplateID); string(m.Message.(*Heartbeat).TestReqID) != "tr" {
		t.Errorf("Heartbeat traced %+v", m)
	}
	p.send(t, tradeOf("BTCUSDT", 1, 1, 0, 0))
	h.next(t, "trade")
	r.wait(t, false, fixsbe.MarketDataIncrementalTradeTemplateID)
	// A template of no message of the schema: its body.
	p.send(t, unknownTemplate{})
	if m := r.wait(t, false, 9999); m.Message != nil || len(m.Body) != 1 || m.Err != nil {
		t.Errorf("unknown template traced %+v", m)
	}
}

// A Logon that does not decode, of either direction, is traced without its
// body: its credentials are in it.
func TestTraceMalformedLogon(t *testing.T) {
	r := &tracer{}
	k := &conn{c: &Client{cfg: Config{Trace: r.trace}}, epoch: 1}
	b, err := (&fixsbe.Logon{HeartBtInt: 30, SenderCompId: []byte("S"), TargetCompId: []byte("SPOT"), RawData: []byte("SECRET-SIGNATURE"),
		Username: []byte("SECRET-API-KEY")}).AppendWithHeader(nil, MessageHeader{SeqNum: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Cut after its signature: the API key is missing.
	i := bytes.Index(b, []byte("SECRET-SIGNATURE")) + len("SECRET-SIGNATURE")
	for _, out := range []bool{true, false} {
		k.trace(out, false, b[:i])
	}
	for _, m := range r.msgs {
		if m.Message != nil || m.Body != nil || m.Err == nil || m.Header.TemplateId != fixsbe.LogonTemplateID {
			t.Errorf("traced %+v", m)
		}
		if bytes.Contains([]byte(fmt.Sprintf("%+v", m)), []byte("SECRET")) {
			t.Errorf("credentials traced: %+v", m)
		}
	}
	if len(r.msgs) != 2 {
		t.Errorf("%d traced", len(r.msgs))
	}
}

// What a failed write leaves: the messages the connection took wholly are
// traced, the one it cut marked Partial, the others not at all.
func TestTraceWrites(t *testing.T) {
	// The requests' IDs, copied during the call: the messages are valid
	// during it only.
	var mu sync.Mutex
	var written, partial []string
	trace := func(m TraceMessage) {
		mu.Lock()
		defer mu.Unlock()
		if mdr, ok := m.Message.(*MarketDataRequest); ok && m.Out {
			if m.Partial {
				partial = append(partial, string(mdr.MDReqID))
			} else {
				written = append(written, string(mdr.MDReqID))
			}
		}
	}
	for _, tc := range []struct {
		name             string
		step             func(frame int) int // what the batch's write takes
		written, partial []string
	}{
		{"partial", func(frame int) int { return frame + 5 }, []string{"BLK", "S0"}, []string{"S1"}},
		{"nothing", func(int) int { return 0 }, []string{"BLK"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			written, partial = nil, nil
			mu.Unlock()
			c, _, h, steps, g := gatedSession(t, func(c *Client) { c.cfg.Trace = trace })
			if _, err := c.Subscribe(trades("BLK", "BNBUSDT")); err != nil {
				t.Fatal(err)
			}
			g.waitWrite(t)
			for i, sym := range []string{"BTCUSDT", "ETHUSDT", "ETHBTC"} {
				if _, err := c.Subscribe(trades(fmt.Sprint("S", i), sym)); err != nil {
					t.Fatal(err)
				}
			}
			steps <- -1
			g.waitWrite(t)
			frame, err := encode(trades("S0", "BTCUSDT"))
			if err != nil {
				t.Fatal(err)
			}
			steps <- tc.step(len(frame))
			h.next(t, "logout")
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(written, tc.written) || !slices.Equal(partial, tc.partial) {
				t.Errorf("traced written %v, partly %v; want %v, %v", written, partial, tc.written, tc.partial)
			}
		})
	}
}

// unknownTemplate is a message of a template the schema does not have.
type unknownTemplate struct{}

func (unknownTemplate) AppendWithHeader(dst []byte, h MessageHeader) ([]byte, error) {
	h.TemplateId, h.SchemaId, h.Version, h.BlockLength = 9999, fixsbe.SchemaID, fixsbe.SchemaVersion, 1
	b, err := (&fixsbe.Heartbeat{}).AppendWithHeader(nil, h)
	if err != nil {
		return nil, err
	}
	// The Heartbeat's header, its template overwritten, then one byte.
	b = b[:fixsbe.HeaderSize]
	b[2], b[3], b[0], b[1] = byte(9999&0xff), byte(9999>>8), 1, 0
	return append(dst, append(b, 0)...), nil
}

// endingHandler waits, on each LogoutEvent, for its Client's Ending.
type endingHandler struct {
	BaseHandler
	c       chan *Client
	waiting chan struct{}
}

func (h *endingHandler) OnLogout(LogoutEvent) {
	c := <-h.c
	h.c <- c
	close(h.waiting)
	<-c.Ending()
}

// Ending is closed once Run connects no more, though it waits for the
// Handler: one waiting for it, as on a blocked output, lets Run return.
func TestEnding(t *testing.T) {
	f := newFake(t)
	h := &endingHandler{c: make(chan *Client, 1), waiting: make(chan struct{})}
	cfg := testConfig(f, h)
	cfg.Reconnect.MaxRefusals = 1
	c, done := start(t, cfg, 50*time.Millisecond)
	h.c <- c
	p := f.accept()
	p.logon(t, &c.cfg)
	p.c.Close()
	<-h.waiting // the Handler waits on Ending
	select {
	case <-c.Ending():
		t.Fatal("Ending closed while reconnecting")
	default:
	}
	p = f.accept()
	p.expect(t, fixsbe.LogonTemplateID)
	p.send(t, &fixsbe.Reject{ErrorCode: Some[int32](-1022), Text: []byte("Signature for this request is not valid.")})
	var refused *LogonRefusedError
	if err := runErr(t, done); !errors.As(err, &refused) {
		t.Errorf("Run: %v", err)
	}
}

// blockedHandler waits, in its first ExecutionReport, for its Client's
// Ending, as a Handler blocked on its output does.
type blockedHandler struct {
	BaseHandler
	c       chan *Client
	waiting chan struct{}
	once    sync.Once
}

func (h *blockedHandler) OnExecutionReport(Inbound, *ExecutionReport) {
	c := <-h.c
	h.c <- c
	h.once.Do(func() { close(h.waiting) })
	<-c.Ending()
}

// The last end of a connection closes Ending before anything waits for
// the Handler: with the event queue full, the reader blocked on it or the
// OnLogout finding no room, and the Handler waiting for Ending, Run
// returns all the same, without its context cancelled. Stop closes it at
// once; a connection ending with reconnection disabled, as its end is
// known, before the reader is joined.
func TestEndingQueueFull(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
		reports  int           // 3: the queue full; 4: the reader blocked on it
		interval time.Duration // the heartbeats': a write ends a lost connection
		end      func(c *Client, p *peer, g *gate)
	}{
		{"Stop", false, 3, time.Hour, func(c *Client, _ *peer, _ *gate) { c.Stop() }},
		{"connection lost, reconnection disabled", true, 3, time.Hour, func(_ *Client, p *peer, _ *gate) { p.c.Close() }},
		{"reader blocked, Stop", false, 4, time.Hour, func(c *Client, _ *peer, _ *gate) { c.Stop() }},
		// The reader blocked sees no end of the connection: the next
		// Heartbeat's write fails.
		{"reader blocked, connection lost, reconnection disabled", true, 4, 100 * time.Millisecond,
			func(_ *Client, p *peer, _ *gate) { p.c.Close() }},
		{"reader blocked, write failure, reconnection disabled", true, 4, 100 * time.Millisecond,
			func(_ *Client, _ *peer, g *gate) { g.release(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			var g *gate
			f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
			h := &blockedHandler{c: make(chan *Client, 1), waiting: make(chan struct{})}
			cfg := testConfig(f, h)
			cfg.MaxEvents = 2
			cfg.Reconnect.Disabled = tc.disabled
			c, done := start(t, cfg, tc.interval)
			h.c <- c
			p := f.accept()
			p.logon(t, &c.cfg)
			// One report in the Handler, two filling the queue (the
			// OnLogout finds no room), a fourth blocking the reader.
			for i := range tc.reports {
				p.send(t, report(fmt.Sprint("r", i)))
			}
			<-h.waiting
			deadline := time.Now().Add(wait)
			for {
				c.mu.Lock()
				k := c.cur
				c.mu.Unlock()
				full := len(c.events) == cap(c.events)
				if full && (tc.reports < 4 || (k != nil && k.stalled.Load())) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("queue holds %d", len(c.events))
				}
				time.Sleep(time.Millisecond)
			}
			tc.end(c, p, g)
			select {
			case <-done:
			case <-time.After(wait):
				t.Fatal("Run did not return")
			}
		})
	}
}

// MaxInitialRefusals bounds the refusals before a first success, and
// MaxRefusals those after.
func TestMaxInitialRefusals(t *testing.T) {
	const interval = 50 * time.Millisecond
	refuse := &fixsbe.Reject{ErrorCode: Some[int32](-1022), Text: []byte("Signature for this request is not valid.")}
	f := newFake(t)
	cfg := testConfig(f, nil)
	cfg.Reconnect.MaxRefusals, cfg.Reconnect.MaxInitialRefusals = 3, 1
	_, done := start(t, cfg, interval)
	p := f.accept()
	p.expect(t, fixsbe.LogonTemplateID)
	p.send(t, refuse)
	var refused *LogonRefusedError
	if err := runErr(t, done); !errors.As(err, &refused) {
		t.Errorf("Run: %v", err)
	}
	if n := len(f.dials()); n != 1 {
		t.Errorf("%d dials, want 1", n)
	}

	// Logged on once: 3 refusals in a row.
	f = newFake(t)
	cfg = testConfig(f, nil)
	cfg.Reconnect.MaxRefusals, cfg.Reconnect.MaxInitialRefusals = 3, 1
	c, done := start(t, cfg, interval)
	p = f.accept()
	p.logon(t, &c.cfg)
	p.c.Close()
	for i := range 3 {
		p = f.accept()
		p.expect(t, fixsbe.LogonTemplateID)
		p.send(t, refuse)
		if i < 2 {
			select {
			case err := <-done:
				t.Fatalf("Run ended after %d refusals: %v", i+1, err)
			default:
			}
		}
	}
	if err := runErr(t, done); !errors.As(err, &refused) {
		t.Errorf("Run: %v", err)
	}
}
