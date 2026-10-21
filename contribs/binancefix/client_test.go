package binancefix

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
	"github.com/sylr/fix2/transport"
)

// The Logon carries the Config's fields and a signature Binance would
// verify; the LogonAck's UUID and deprecation reach OnLogon.
func TestLogon(t *testing.T) {
	f, h := newFake(t), newRecorder()
	cfg := testConfig(f, h)
	cfg.HeartBtInt = 10 * time.Second
	cfg.MessageHandling = fixsbe.MessageHandlingUnordered
	cfg.ResponseMode = fixsbe.ResponseModeOnlyAcks
	cfg.ExecutionReportType = fixsbe.ExecutionReportTypeMini
	cfg.RecvWindow = 6000346 * time.Microsecond
	c, _ := start(t, cfg, time.Hour)
	p := f.accept()
	m := p.expect(t, fixsbe.LogonTemplateID)
	l := checkLogon(t, m, &cfg)
	if l.HeartBtInt != 10 || l.MessageHandling != Some(fixsbe.MessageHandlingUnordered) || l.ResponseMode != Some(fixsbe.ResponseModeOnlyAcks) ||
		l.ExecutionReportType != Some(fixsbe.ExecutionReportTypeMini) || l.RecvWindow != Some[uint32](6000346) {
		t.Errorf("Logon %+v", l)
	}
	if c.LoggedOn() {
		t.Error("logged on before the LogonAck")
	}
	if _, err := c.Send(order("o1")); !errors.Is(err, ErrNotLoggedOn) {
		t.Errorf("Send before the LogonAck: %v", err)
	}
	p.send(t, &fixsbe.LogonAck{EncryptMethod: Some[uint8](0), HeartBtInt: 10, SbeSchemaIdVersionDeprecated: fixsbe.BoolEnumTrue, UUID: []byte("4392a152")})
	e := h.next(t, "logon").ev.(LogonEvent)
	if e.Epoch != 1 || e.UUID != "4392a152" || !e.SchemaDeprecated || e.HeartBtInt != 10*time.Second {
		t.Errorf("LogonEvent %+v", e)
	}
	if !c.LoggedOn() || c.Epoch() != 1 {
		t.Error("not logged on")
	}
}

// The defaults: MessageHandling SEQUENTIAL, no ResponseMode, RecvWindow
// or ExecutionReportType.
func TestLogonDefaults(t *testing.T) {
	f := newFake(t)
	cfg := testConfig(f, nil)
	start(t, cfg, time.Hour)
	p := f.accept()
	l := checkLogon(t, p.expect(t, fixsbe.LogonTemplateID), &Config{SenderCompID: "TEST1", APIKey: "api-key", HeartBtInt: DefaultHeartBtInt})
	if l.MessageHandling != Some(fixsbe.MessageHandlingSequential) || l.ResponseMode.Valid || l.RecvWindow.Valid || l.ExecutionReportType.Valid {
		t.Errorf("Logon %+v", l)
	}
}

// A refused Logon is retried after twice HeartBtInt, MaxRefusals times in
// a row at most: then Run ends.
func TestLogonRefused(t *testing.T) {
	const interval = 50 * time.Millisecond
	for _, refusal := range []encoder{
		&fixsbe.Reject{ErrorCode: Some[int32](-1022), Text: []byte("Signature for this request is not valid."), RefMsgType: []byte("A")},
		&fixsbe.Logout{Text: []byte("Invalid API key")},
	} {
		f := newFake(t)
		cfg := testConfig(f, nil)
		cfg.Reconnect.MaxRefusals = 2
		_, done := start(t, cfg, interval)
		for range 2 {
			p := f.accept()
			p.expect(t, fixsbe.LogonTemplateID)
			p.send(t, refusal)
		}
		var refused *LogonRefusedError
		if err := runErr(t, done); !errors.As(err, &refused) {
			t.Errorf("Run: %v", err)
		}
		if ds := f.dials(); len(ds) != 2 || ds[1].Sub(ds[0]) < 2*interval {
			t.Errorf("dials %v", ds)
		}
	}
}

// Every message after the Logon is numbered from 2, session messages and
// requests alike, and stamped as it is written.
func TestSequenceNumbers(t *testing.T) {
	f, h := newFake(t), newRecorder()
	c, _ := start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	before := time.Now()
	for _, id := range []string{"a", "b"} {
		if _, err := c.Send(order(id)); err != nil {
			t.Fatal(err)
		}
	}
	p.send(t, &fixsbe.TestRequest{TestReqID: []byte("t1")})
	var seqs []uint32
	for len(seqs) < 3 {
		m := p.next(t)
		seqs = append(seqs, m.h.SeqNum)
		if st := time.UnixMicro(m.h.SendingTime); st.Before(before.Truncate(time.Microsecond)) || st.After(time.Now()) {
			t.Errorf("sendingTime %v, not when written", st)
		}
	}
	if seqs[0] != 2 || seqs[1] != 3 || seqs[2] != 4 {
		t.Errorf("seqNums %v, want 2 3 4", seqs)
	}
}

// Sequence numbers roll over from 4,294,967,295 to 0, both ways.
func TestSequenceRollover(t *testing.T) {
	f, h := newFake(t), newRecorder()
	c, _ := start(t, testConfig(f, h), time.Hour, func(c *Client) {
		c.nextOutAfterLogon, c.nextInAfterLogon = math.MaxUint32, math.MaxUint32
	})
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	for _, id := range []string{"a", "b", "c"} {
		if _, err := c.Send(order(id)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []uint32{math.MaxUint32, 0, 1} {
		if m := p.expect(t, fixsbe.NewOrderSingleTemplateID); m.h.SeqNum != want {
			t.Errorf("seqNum %d, want %d", m.h.SeqNum, want)
		}
	}
	p.seq = math.MaxUint32
	for _, id := range []string{"a", "b", "c"} {
		p.send(t, report(id))
		if r := h.next(t, "er"); r.in.Header.SeqNum != p.seq-1 || r.in.Receipt == nil {
			t.Errorf("ExecutionReport %d: %+v", p.seq-1, r.in)
		}
	}
	if p.seq != 2 {
		t.Fatalf("seq %d", p.seq)
	}
}

// A gap, or a number already seen, ends the connection after our Logout:
// Binance resends nothing.
func TestSequenceGap(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  uint32
	}{{"gap", 3}, {"too low", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := newFake(t), newRecorder()
			cfg := testConfig(f, h)
			cfg.Reconnect.Disabled = true
			c, done := start(t, cfg, time.Hour)
			p := f.accept()
			p.logon(t, &c.cfg)
			h.next(t, "logon")
			r, err := c.Send(order("o1"))
			if err != nil {
				t.Fatal(err)
			}
			p.expect(t, fixsbe.NewOrderSingleTemplateID)
			p.sendSeq(t, report("o1"), tc.seq)
			lo := decode[fixsbe.Logout](t, p.expect(t, fixsbe.LogoutTemplateID))
			if !bytes.Contains(lo.Text, []byte("sequence number")) {
				t.Errorf("Logout %q", lo.Text)
			}
			p.ended(t)
			e := h.next(t, "logout").ev.(LogoutEvent)
			var se *SequenceError
			if !errors.As(e.Err, &se) || se.Expected != 2 || se.Got != tc.seq || !e.Clean {
				t.Errorf("LogoutEvent %+v", e)
			}
			if r.State() != PossiblyTransmitted || len(e.PossiblyTransmitted) != 1 || e.PossiblyTransmitted[0] != r {
				t.Errorf("receipt %v, event %s", r.State(), describe(e.PossiblyTransmitted))
			}
			if err := runErr(t, done); !errors.As(err, &se) {
				t.Errorf("Run: %v", err)
			}
		})
	}
}

// We heartbeat after HeartBtInt without writing, answer TestRequests,
// send a TestRequest after HeartBtInt without reading, and end the
// connection when it goes unanswered.
func TestHeartbeats(t *testing.T) {
	const interval = 100 * time.Millisecond
	f, h := newFake(t), newRecorder()
	cfg := testConfig(f, h)
	cfg.Reconnect.Disabled = true
	c, done := start(t, cfg, interval)
	p := f.accept()
	loggedOn := time.Now()
	p.logon(t, &c.cfg)
	h.next(t, "logon")

	// Silent, the peer gets a TestRequest, and our Heartbeats.
	var test *fixsbe.TestRequest
	heartbeats := 0
	for test == nil {
		m := p.next(t)
		switch m.h.TemplateId {
		case fixsbe.HeartbeatTemplateID:
			heartbeats++
		case fixsbe.TestRequestTemplateID:
			test = decode[fixsbe.TestRequest](t, m)
			if d := m.at.Sub(loggedOn); d < interval-interval/8 {
				t.Errorf("TestRequest after %v, HeartBtInt %v", d, interval)
			}
		default:
			t.Fatalf("template %d", m.h.TemplateId)
		}
	}
	if heartbeats == 0 {
		t.Error("no Heartbeat")
	}
	p.send(t, &fixsbe.Heartbeat{TestReqID: bytes.Clone(test.TestReqID)})

	// Our TestRequest answered, the connection goes on; theirs is
	// answered.
	p.send(t, &fixsbe.TestRequest{TestReqID: []byte("theirs")})
	for {
		m := p.next(t)
		if m.h.TemplateId == fixsbe.HeartbeatTemplateID {
			if hb := decode[fixsbe.Heartbeat](t, m); string(hb.TestReqID) == "theirs" {
				break
			}
		}
	}

	// The next TestRequest unanswered, the connection ends after our
	// Logout.
	var silentAt time.Time
	for {
		m := p.next(t)
		if m.h.TemplateId == fixsbe.TestRequestTemplateID {
			silentAt = m.at
		}
		if m.h.TemplateId == fixsbe.LogoutTemplateID {
			if silentAt.IsZero() {
				t.Fatal("Logout before a TestRequest")
			}
			if d := m.at.Sub(silentAt); d < interval-interval/8 {
				t.Errorf("Logout %v after the TestRequest", d)
			}
			break
		}
	}
	p.ended(t)
	e := h.next(t, "logout").ev.(LogoutEvent)
	if !errors.Is(e.Err, ErrPeerSilent) {
		t.Errorf("LogoutEvent %+v", e)
	}
	if err := runErr(t, done); !errors.Is(err, ErrPeerSilent) {
		t.Errorf("Run: %v", err)
	}
}

// Binance's Logout is answered with ours; requests still queued are
// NeverWritten, and the Client reconnects.
func TestPeerLogout(t *testing.T) {
	f, h := newFake(t), newRecorder()
	c, _ := start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	p.send(t, &fixsbe.Logout{Text: []byte("bye")})
	p.expect(t, fixsbe.LogoutTemplateID)
	p.ended(t)
	e := h.next(t, "logout").ev.(LogoutEvent)
	var pl *PeerLogoutError
	if !errors.As(e.Err, &pl) || pl.Text != "bye" || !e.Clean {
		t.Errorf("LogoutEvent %+v", e)
	}
	// Reconnected, without the cooldown of an unclean end.
	p2 := f.accept()
	p2.logon(t, &c.cfg)
	if e := h.next(t, "logon").ev.(LogonEvent); e.Epoch != 2 {
		t.Errorf("LogonEvent %+v", e)
	}
}

// Stop logs out: Logout, Binance's answer, Run returns nil. Requests
// still queued are NeverWritten, those written and unanswered
// PossiblyTransmitted; the session messages pass the full request queue.
func TestStop(t *testing.T) {
	f, h := newFake(t), newRecorder()
	cfg := testConfig(f, h)
	cfg.MaxQueued = 2
	var g *gate
	f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
	c, done := start(t, cfg, time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	g.shut()

	a, err := c.Send(order("a"))
	if err != nil {
		t.Fatal(err)
	}
	g.waitWrite(t) // a is being written
	b, err1 := c.Send(order("b"))
	cc, err2 := c.Send(order("c"))
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if _, err := c.Send(order("d")); !errors.Is(err, ErrQueueFull) {
		t.Errorf("Send beyond MaxQueued: %v", err)
	}
	c.Stop()
	for c.LoggedOn() {
		time.Sleep(time.Millisecond)
	}
	if _, err := c.Send(order("e")); !errors.Is(err, ErrNotLoggedOn) {
		t.Errorf("Send after Stop: %v", err)
	}
	g.release(0)
	p.expect(t, fixsbe.NewOrderSingleTemplateID)
	p.expect(t, fixsbe.LogoutTemplateID)
	p.send(t, &fixsbe.Logout{Text: []byte("Logout acknowledgment.")})
	p.ended(t)
	e := h.next(t, "logout").ev.(LogoutEvent)
	if e.Err != nil || !e.Clean {
		t.Errorf("LogoutEvent %+v", e)
	}
	if describe(e.NeverWritten) != describe([]*Receipt{b, cc}) || b.State() != NeverWritten || cc.State() != NeverWritten {
		t.Errorf("NeverWritten %s", describe(e.NeverWritten))
	}
	if len(e.PossiblyTransmitted) != 1 || e.PossiblyTransmitted[0] != a || a.State() != PossiblyTransmitted {
		t.Errorf("PossiblyTransmitted %s", describe(e.PossiblyTransmitted))
	}
	if err := runErr(t, done); err != nil {
		t.Errorf("Run: %v", err)
	}
}

// Stop from OnLogon logs out, the connection finding the LogonAck and the
// Stop both there or not: logged on, it is ended by a Logout.
func TestStopFromOnLogon(t *testing.T) {
	for range 50 {
		f, h := newFake(t), newRecorder()
		c, done := start(t, testConfig(f, h), time.Hour, func(c *Client) {
			h.onLogon = func(LogonEvent) { c.Stop() }
		})
		p := f.accept()
		p.logon(t, &c.cfg)
		p.expect(t, fixsbe.LogoutTemplateID)
		p.send(t, &fixsbe.Logout{Text: []byte("Logout acknowledgment.")})
		p.ended(t)
		if err := runErr(t, done); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
}

// Run's context done logs out too.
func TestContextDone(t *testing.T) {
	f, h := newFake(t), newRecorder()
	c, err := New(testConfig(f, h))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	cancel()
	p.expect(t, fixsbe.LogoutTemplateID)
	p.send(t, &fixsbe.Logout{})
	if err := runErr(t, done); err != nil {
		t.Errorf("Run: %v", err)
	}
	if err := c.Run(ctx); !errors.Is(err, ErrRunning) {
		t.Errorf("Run again: %v", err)
	}
}

// A request's receipt across its connection's phases: answered, written
// and unanswered, partly written, never written.
func TestReceipts(t *testing.T) {
	f, h := newFake(t), newRecorder()
	var g *gate
	f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
	c, _ := start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")

	// Answered by its ExecutionReport, matched by ClOrdID; a later one
	// carries no receipt.
	a, err := c.Send(order("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(order("a")); !errors.Is(err, ErrDuplicateID) {
		t.Errorf("Send of a ClOrdID awaiting its answer: %v", err)
	}
	m := p.expect(t, fixsbe.NewOrderSingleTemplateID)
	if seq, ok := a.SeqNum(); !ok || seq != m.h.SeqNum {
		t.Errorf("SeqNum %d, %v; sent as %d", seq, ok, m.h.SeqNum)
	}
	p.send(t, report("a"))
	if r := h.next(t, "er"); r.in.Receipt != a || r.id != "a" {
		t.Errorf("ExecutionReport %+v", r)
	}
	if st, err := a.Wait(context.Background()); st != Answered || err != nil {
		t.Errorf("Wait = %v, %v", st, err)
	}
	if ans, ok := a.Answer(); !ok || ans.TemplateID != fixsbe.ExecutionReportTemplateID {
		t.Errorf("Answer %+v", ans)
	}
	p.send(t, report("a"))
	if r := h.next(t, "er"); r.in.Receipt != nil {
		t.Errorf("a second answer matched: %+v", r)
	}

	// -1007: status unknown.
	u, _ := c.Send(order("u"))
	p.expect(t, fixsbe.NewOrderSingleTemplateID)
	er := report("u")
	er.ExecType, er.OrdStatus, er.ErrorCode = fixsbe.ExecTypeRejected, fixsbe.OrdStatusRejected, Some[int32](-1007)
	p.send(t, er)
	h.next(t, "er")
	if u.State() != StatusUnknown {
		t.Errorf("-1007: %v", u.State())
	}

	// A Reject, by RefSeqNum.
	rj, _ := c.Send(order("rj"))
	m = p.expect(t, fixsbe.NewOrderSingleTemplateID)
	p.send(t, &fixsbe.Reject{RefSeqNum: Some(m.h.SeqNum), ErrorCode: Some[int32](-1221), RefMsgType: []byte{}, Text: []byte("bad")})
	if r := h.next(t, "reject"); r.in.Receipt != rj || rj.State() != Answered {
		t.Errorf("Reject %+v, %v", r, rj.State())
	}

	// A NewOrderList by ListStatus's ClListID, a LimitQuery by ReqID, an
	// OrderCancelRequest by OrderCancelReject's ClOrdID.
	l, err := c.Send(&NewOrderList{ContingencyType: fixsbe.ContingencyTypeOneCancelsTheOther, ClListID: []byte("L1"),
		Orders: []NewOrderListOrders{{OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideSell, ClOrdID: []byte("L1a"), Symbol: []byte("BTCUSDT")},
			{OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideSell, ClOrdID: []byte("L1b"), Symbol: []byte("BTCUSDT")}}})
	if err != nil {
		t.Fatal(err)
	}
	q, _ := c.Send(&LimitQuery{ReqID: []byte("q1")})
	x, _ := c.Send(&OrderCancelRequest{ClOrdID: []byte("x1"), OrigClOrdID: []byte("a"), Symbol: []byte("BTCUSDT"), OrigClListID: []byte{}})
	p.expect(t, fixsbe.NewOrderListTemplateID)
	p.expect(t, fixsbe.LimitQueryTemplateID)
	p.expect(t, fixsbe.OrderCancelRequestTemplateID)
	p.send(t, &fixsbe.ListStatus{ListStatusType: fixsbe.ListStatusTypeExecStarted, ListOrderStatus: fixsbe.ListOrderStatusExecuting, ClListID: []byte("L1"), OrigClListID: []byte{},
		Orders: []ListStatusOrders{{OrderID: Some[int64](11), ClOrdID: []byte("L1a"), Symbol: []byte("BTCUSDT"), ErrorText: []byte{}},
			{OrderID: Some[int64](12), ClOrdID: []byte("L1b"), Symbol: []byte("BTCUSDT"), ErrorText: []byte{}}}})
	p.send(t, &fixsbe.LimitResponse{ReqID: []byte("q1")})
	p.send(t, &fixsbe.OrderCancelReject{CxlRejResponseTo: fixsbe.CxlRejResponseToOrderCancelRequest, ErrorCode: -2011,
		ClOrdID: []byte("x1"), OrigClOrdID: []byte("a"), OrigClListID: []byte{}, Symbol: []byte("BTCUSDT"), ErrorText: []byte("Unknown order sent.")})
	if r := h.next(t, "list"); r.in.Receipt != l {
		t.Errorf("ListStatus %+v", r)
	}
	if r := h.next(t, "limits"); r.in.Receipt != q {
		t.Errorf("LimitResponse %+v", r)
	}
	if r := h.next(t, "cancelreject"); r.in.Receipt != x {
		t.Errorf("OrderCancelReject %+v", r)
	}
	if ans, _ := x.Answer(); ans.ErrorCode != Some[int32](-2011) {
		t.Errorf("Answer %+v", ans)
	}

	// Written, unanswered, then the connection ends: PossiblyTransmitted.
	w, _ := c.Send(order("w"))
	p.expect(t, fixsbe.NewOrderSingleTemplateID)
	waitState(t, w, Written)
	// Partly written: the write fails after the first request's first
	// bytes, the second never reaching the connection.
	g.shut()
	pa, _ := c.Send(order("pa"))
	g.waitWrite(t)
	nb, _ := c.Send(order("nb"))
	nc, _ := c.Send(order("nc"))
	g.release(10)
	e := h.next(t, "logout").ev.(LogoutEvent)
	if e.Err == nil || e.Clean {
		t.Errorf("LogoutEvent %+v", e)
	}
	if describe(e.PossiblyTransmitted) != describe([]*Receipt{w, pa}) {
		t.Errorf("PossiblyTransmitted %s", describe(e.PossiblyTransmitted))
	}
	if describe(e.NeverWritten) != describe([]*Receipt{nb, nc}) {
		t.Errorf("NeverWritten %s", describe(e.NeverWritten))
	}
	for _, r := range []*Receipt{w, pa, nb, nc} {
		if r.Err() == nil || r.Epoch() != 1 {
			t.Errorf("%v: %v, epoch %d", r.IDs(), r.Err(), r.Epoch())
		}
	}
	if _, err := c.Send(order("late")); !errors.Is(err, ErrNotLoggedOn) {
		t.Errorf("Send between connections: %v", err)
	}
}

// waitState waits for r to reach state s.
func waitState(t *testing.T, r *Receipt, s State) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for r.State() != s {
		if time.Now().After(deadline) {
			t.Fatalf("state %v, want %v", r.State(), s)
		}
		time.Sleep(time.Millisecond)
	}
}

// A write failing before a byte of a batch is written leaves its requests
// NeverWritten; an answer never matches a request not written.
func TestReceiptsNeverWritten(t *testing.T) {
	f, h := newFake(t), newRecorder()
	var g *gate
	f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
	c, _ := start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	g.shut()
	a, _ := c.Send(order("a"))
	g.waitWrite(t)
	b, _ := c.Send(order("b"))
	// An ExecutionReport of b's ClOrdID, from elsewhere: b is not written.
	p.send(t, report("b"))
	if r := h.next(t, "er"); r.in.Receipt != nil {
		t.Errorf("matched a request not written: %+v", r)
	}
	p.c.Close()
	g.Close()
	e := h.next(t, "logout").ev.(LogoutEvent)
	if a.State() != NeverWritten || b.State() != NeverWritten || describe(e.NeverWritten) != describe([]*Receipt{a, b}) || len(e.PossiblyTransmitted) != 0 {
		t.Errorf("a %v, b %v; event %+v", a.State(), b.State(), e)
	}
}

// Reject and News reach the Handler; News tells the time left, and
// Reconnect from the Handler moves the session with a clean Logout.
func TestRejectAndNews(t *testing.T) {
	f, h := newFake(t), newRecorder()
	var c *Client
	h.onNews = func(e NewsEvent) {
		c.Reconnect(e.Epoch + 1) // not this connection's: ignored
		c.Reconnect(e.Epoch)
	}
	c, _ = start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	p.send(t, &fixsbe.Reject{RefSeqNum: Some[uint32](1), ErrorCode: Some[int32](-1000), RefMsgType: []byte{}, Text: []byte("?")})
	if r := h.next(t, "reject"); r.in.Receipt != nil || r.code != -1000 {
		t.Errorf("Reject %+v", r)
	}
	p.send(t, &fixsbe.News{Headline: []byte("You'll be disconnected in 30 seconds. Please reconnect.")})
	if e := h.next(t, "news").ev.(NewsEvent); e.Remaining != 30*time.Second || e.Epoch != 1 {
		t.Errorf("NewsEvent %+v", e)
	}
	p.expect(t, fixsbe.LogoutTemplateID)
	p.send(t, &fixsbe.Logout{})
	if e := h.next(t, "logout").ev.(LogoutEvent); e.Err != nil || !e.Clean {
		t.Errorf("LogoutEvent %+v", e)
	}
	p2 := f.accept()
	p2.logon(t, &c.cfg)
	if e := h.next(t, "logon").ev.(LogonEvent); e.Epoch != 2 {
		t.Errorf("LogonEvent %+v", e)
	}
	if d := remaining("Your connection is about to be closed. Please reconnect."); d != 10*time.Second {
		t.Errorf("remaining %v", d)
	}
}

// Send works from the Handler's methods.
func TestSendFromHandler(t *testing.T) {
	f, h := newFake(t), newRecorder()
	var c *Client
	sent := make(chan error, 2)
	h.onLogon = func(LogonEvent) {
		_, err := c.Send(&LimitQuery{ReqID: []byte("q1")})
		sent <- err
	}
	h.onLimits = func(Inbound, *LimitResponse) {
		_, err := c.Send(order("o1"))
		sent <- err
	}
	c, _ = start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	p.expect(t, fixsbe.LimitQueryTemplateID)
	p.send(t, &fixsbe.LimitResponse{ReqID: []byte("q1")})
	if o := decode[fixsbe.NewOrderSingle](t, p.expect(t, fixsbe.NewOrderSingleTemplateID)); string(o.ClOrdID) != "o1" {
		t.Errorf("order %q", o.ClOrdID)
	}
	for range 2 {
		if err := <-sent; err != nil {
			t.Error(err)
		}
	}
}

// A Handler stalling does not stall our Heartbeats, nor our answers to
// TestRequests while the event queue has room; once it is full, reading
// waits, which is not taken for the peer's silence.
func TestHandlerStall(t *testing.T) {
	const interval = 100 * time.Millisecond
	f, h := newFake(t), newRecorder()
	release := make(chan struct{})
	h.onER = func(Inbound, *ExecutionReport) { <-release }
	cfg := testConfig(f, h)
	cfg.MaxEvents = 2
	c, _ := start(t, cfg, interval)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	p.send(t, report("x")) // the Handler stalls on it
	// The event queue has room: TestRequests are answered.
	p.send(t, &fixsbe.TestRequest{TestReqID: []byte("t1")})
	for {
		m := p.next(t)
		if m.h.TemplateId == fixsbe.HeartbeatTemplateID && string(decode[fixsbe.Heartbeat](t, m).TestReqID) == "t1" {
			break
		}
	}
	// Fill the queue: reading waits; our Heartbeats go on for several
	// intervals, and the connection is not ended for our not reading.
	for range 4 {
		p.send(t, report("y"))
	}
	until := time.Now().Add(5 * interval)
	heartbeats := 0
	for time.Now().Before(until) {
		select {
		case m, ok := <-p.msgs:
			if !ok {
				t.Fatal("connection ended")
			}
			switch m.h.TemplateId {
			case fixsbe.HeartbeatTemplateID:
				heartbeats++
			case fixsbe.LogoutTemplateID:
				t.Fatal("Logout while the Handler stalls")
			}
		case <-time.After(interval):
		}
	}
	if heartbeats < 3 {
		t.Errorf("%d Heartbeats in 5 intervals", heartbeats)
	}
	close(release)
	for range 5 {
		h.next(t, "er")
	}
}

// A frame Binance's framing refuses ends the connection.
func TestBadFrames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"big-endian encoding", []byte{26, 0, 0, 0, 0xEB, 0x50}},
		{"short length", []byte{5, 0, 0, 0, 0x50, 0xEB}},
		{"oversized", []byte{1, 0, 1, 0, 0x50, 0xEB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := newFake(t), newRecorder()
			cfg := testConfig(f, h)
			cfg.Reconnect.Disabled = true
			c, done := start(t, cfg, time.Hour)
			p := f.accept()
			p.logon(t, &c.cfg)
			h.next(t, "logon")
			p.write(t, tc.frame)
			var pe *ProtocolError
			if err := runErr(t, done); !errors.As(err, &pe) {
				t.Errorf("Run: %v", err)
			}
		})
	}
	// Fragmented and coalesced frames are read whole.
	f, h := newFake(t), newRecorder()
	c, _ := start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.expect(t, fixsbe.LogonTemplateID)
	var stream []byte
	for i, m := range []encoder{&fixsbe.LogonAck{HeartBtInt: 30, UUID: []byte("u")}, report("a"), report("b")} {
		b, _ := m.AppendWithHeader(nil, MessageHeader{SeqNum: uint32(i + 1)})
		stream, _ = lesofh.AppendFrame(stream, b)
	}
	for i := range stream {
		p.write(t, stream[i:i+1])
	}
	h.next(t, "logon")
	if r := h.next(t, "er"); r.id != "a" {
		t.Errorf("first %q", r.id)
	}
	if r := h.next(t, "er"); r.id != "b" {
		t.Errorf("second %q", r.id)
	}
	_ = c
}

// After a session ended without our Logout, the next Logon waits twice
// HeartBtInt; after a clean one, only the backoff.
func TestReconnectCooldown(t *testing.T) {
	const interval = 100 * time.Millisecond
	f, h := newFake(t), newRecorder()
	c, _ := start(t, testConfig(f, h), interval)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	p.c.Close() // no Logout
	h.next(t, "logout")
	ended := time.Now()
	p2 := f.accept()
	ds := f.dials()
	if d := ds[len(ds)-1].Sub(ended); d < 2*interval-interval/4 {
		t.Errorf("reconnected %v after an unclean end, want %v", d, 2*interval)
	}
	p2.logon(t, &c.cfg)
	h.next(t, "logon")
}

// Send refuses what is not a request, and IDs Binance would refuse.
func TestSendRefusals(t *testing.T) {
	c, err := New(testConfig(newFake(t), nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		m    Request
		want error
	}{
		{&fixsbe.Heartbeat{}, ErrNotRequest},
		{order(""), ErrInvalidID},
		{order("has space"), ErrInvalidID},
		{order("0123456789012345678901234567890123456"), ErrInvalidID},
		{&NewOrderList{ClListID: []byte("L"), Orders: []NewOrderListOrders{{ClOrdID: []byte("L")}}}, ErrDuplicateID},
		{order("ok"), ErrNotLoggedOn},
	} {
		if _, err := c.Send(tc.m); !errors.Is(err, tc.want) {
			t.Errorf("Send(%T) = %v, want %v", tc.m, err, tc.want)
		}
	}
}

func TestConfig(t *testing.T) {
	good := Config{APIKey: "k", PrivateKey: testKey(t), SenderCompID: "S"}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for _, mod := range []func(*Config){
		func(c *Config) { c.APIKey = "" },
		func(c *Config) { c.PrivateKey = nil },
		func(c *Config) { c.SenderCompID = "TOOLONGID" },
		func(c *Config) { c.SenderCompID = "" },
		func(c *Config) { c.HeartBtInt = 4 * time.Second },
		func(c *Config) { c.HeartBtInt = 61 * time.Second },
		func(c *Config) { c.HeartBtInt = 5500 * time.Millisecond },
		func(c *Config) { c.RecvWindow = 61 * time.Second },
		func(c *Config) { c.MessageHandling = 3 },
	} {
		cfg := good
		mod(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) accepted", cfg)
		}
	}
}

// A write failing part way splits its batch: the requests begun before the
// failure are PossiblyTransmitted, those wholly after it NeverWritten.
func TestPartialWrite(t *testing.T) {
	f, h := newFake(t), newRecorder()
	var g *gate
	f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
	c, _ := start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	steps := make(chan int)
	g.mu.Lock()
	g.steps = steps
	g.mu.Unlock()
	blk, _ := c.Send(order("blk"))
	g.waitWrite(t) // blk alone in the writer's hands
	a, _ := c.Send(order("a"))
	b, _ := c.Send(order("b"))
	cc, _ := c.Send(order("c"))
	steps <- -1 // blk written
	g.waitWrite(t)
	frame, err := encode(order("a"))
	if err != nil {
		t.Fatal(err)
	}
	steps <- len(frame) + 5 // a wholly, b partly, c not at all
	e := h.next(t, "logout").ev.(LogoutEvent)
	if describe(e.PossiblyTransmitted) != describe([]*Receipt{blk, a, b}) {
		t.Errorf("PossiblyTransmitted %s", describe(e.PossiblyTransmitted))
	}
	if describe(e.NeverWritten) != describe([]*Receipt{cc}) {
		t.Errorf("NeverWritten %s", describe(e.NeverWritten))
	}
}

// A Handler stalling while our TestRequest awaits its answer: the time
// stalled is not counted as the peer's silence.
func TestHandlerStallTestRequest(t *testing.T) {
	const interval = 100 * time.Millisecond
	f, h := newFake(t), newRecorder()
	release := make(chan struct{})
	// Once released, the Handler stays slow: reading stalls again and
	// again while the backlog drains.
	h.onER = func(Inbound, *ExecutionReport) { <-release; time.Sleep(interval / 10) }
	cfg := testConfig(f, h)
	cfg.MaxEvents = 1
	c, _ := start(t, cfg, interval)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	test := decode[fixsbe.TestRequest](t, p.expect(t, fixsbe.TestRequestTemplateID))
	// Unanswered yet, the Handler stalls and reading waits.
	for range 10 {
		p.send(t, report("x"))
	}
	p.send(t, &fixsbe.Heartbeat{TestReqID: bytes.Clone(test.TestReqID)})
	time.Sleep(3 * interval)
	close(release)
	// The answer read, the connection goes on.
	until := time.Now().Add(3 * interval)
	for time.Now().Before(until) {
		select {
		case m := <-p.msgs:
			switch m.h.TemplateId {
			case fixsbe.LogoutTemplateID:
				t.Fatalf("Logout: %q", decode[fixsbe.Logout](t, m).Text)
			case fixsbe.TestRequestTemplateID:
				tr := decode[fixsbe.TestRequest](t, m)
				p.send(t, &fixsbe.Heartbeat{TestReqID: bytes.Clone(tr.TestReqID)})
			}
		case <-time.After(interval / 4):
		}
	}
	if !c.LoggedOn() {
		t.Error("connection ended")
	}
}
