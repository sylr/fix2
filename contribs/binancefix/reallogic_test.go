package binancefix

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
)

// rlFrames returns the frames Real Logic's SBE codecs encoded from
// Binance's schema, framed in Binance's SOFH, by name
// (testdata/reallogic/binance-sofh.hex, a copy of fix2's
// internal/gen/sbe/testdata/reallogic, where BinanceCheck.java.txt tells
// their values).
func rlFrames(t *testing.T) map[string][]byte {
	t.Helper()
	f, err := os.Open("testdata/reallogic/binance-sofh.hex")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	frames := map[string][]byte{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		name, hx, _ := strings.Cut(sc.Text(), " ")
		b, err := hex.DecodeString(hx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		frames[name] = b
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return frames
}

// rlEvent is what rlHandler records: a message encoded again with its
// header, or an event.
type rlEvent struct {
	kind string
	in   Inbound
	b    []byte
	ev   any
	err  error
}

// rlHandler records every event in order, each message encoded again from
// what the client decoded, with the header it read.
type rlHandler struct {
	BaseHandler
	evs chan rlEvent
}

func (h *rlHandler) put(kind string, in Inbound, m encoder) {
	b, err := m.AppendWithHeader(nil, in.Header)
	h.evs <- rlEvent{kind: kind, in: in, b: b, err: err}
}

func (h *rlHandler) OnLogon(e LogonEvent)   { h.evs <- rlEvent{kind: "logon", ev: e} }
func (h *rlHandler) OnLogout(e LogoutEvent) { h.evs <- rlEvent{kind: "logout", ev: e} }
func (h *rlHandler) OnNews(e NewsEvent)     { h.evs <- rlEvent{kind: "news", in: e.Inbound, ev: e} }
func (h *rlHandler) OnReject(in Inbound, m *Reject) {
	h.put("Reject", in, m)
}
func (h *rlHandler) OnExecutionReport(in Inbound, m *ExecutionReport) {
	h.put("ExecutionReport", in, m)
}
func (h *rlHandler) OnExecutionReportAck(in Inbound, m *ExecutionReportAck) {
	h.put("ExecutionReportAck", in, m)
}
func (h *rlHandler) OnOrderCancelReject(in Inbound, m *OrderCancelReject) {
	h.put("OrderCancelReject", in, m)
}
func (h *rlHandler) OnOrderAmendReject(in Inbound, m *OrderAmendReject) {
	h.put("OrderAmendReject", in, m)
}
func (h *rlHandler) OnListStatus(in Inbound, m *ListStatus) { h.put("ListStatus", in, m) }
func (h *rlHandler) OnOrderMassCancelReport(in Inbound, m *OrderMassCancelReport) {
	h.put("OrderMassCancelReport", in, m)
}
func (h *rlHandler) OnLimitResponse(in Inbound, m *LimitResponse) { h.put("LimitResponse", in, m) }
func (h *rlHandler) OnUnknown(in Inbound, body []byte, err error) {
	h.evs <- rlEvent{kind: "unknown", in: in, b: bytes.Clone(body), err: err}
}

func (h *rlHandler) next(t *testing.T) rlEvent {
	t.Helper()
	select {
	case e := <-h.evs:
		return e
	case <-time.After(wait):
		t.Fatal("no event")
		return rlEvent{}
	}
}

// A fake Binance answering with Real Logic's bytes: the client takes the
// LogonAck, answers the TestRequest, hands every message to the Handler
// with the values Real Logic encoded (encoded again, they are its bytes),
// answers its requests' receipts by them, and passes a template it does
// not handle to OnUnknown. Its requests, of the values Real Logic encoded,
// are Real Logic's bytes but for the seqNum and sendingTime it stamps.
// Only the frames' seqNums are rewritten, to follow each other from 1.
func TestRealLogicPeer(t *testing.T) {
	frames := rlFrames(t)
	f, h := newFake(t), &rlHandler{evs: make(chan rlEvent, 256)}
	cfg := testConfig(f, h)
	cfg.Reconnect.Disabled = true
	c, done := start(t, cfg, time.Hour)
	p := f.accept()
	checkLogon(t, p.expect(t, fixsbe.LogonTemplateID), &c.cfg)

	// serve writes the frame of name with the next seqNum, and returns its
	// message as written.
	serve := func(name string) []byte {
		t.Helper()
		fr := bytes.Clone(frames[name])
		if fr == nil {
			t.Fatalf("no fixture %s", name)
		}
		binary.LittleEndian.PutUint32(fr[seqNumOffset:], p.seq)
		p.seq++
		p.write(t, fr)
		return fr[lesofh.HeaderLen:]
	}

	serve("LogonAck")
	if e := h.next(t); e.kind != "logon" {
		t.Fatalf("event %+v", e)
	} else if le := e.ev.(LogonEvent); le.Epoch != 1 || le.HeartBtInt != 30*time.Second ||
		le.UUID != "4392a152-0e2c-4b9b-a6a3-0f1c2d3e4f50" || !le.SchemaDeprecated {
		t.Errorf("LogonEvent %+v", le)
	}

	// Session messages: Heartbeats taken, the TestRequest answered, the
	// Rejects and the News handed over.
	serve("Heartbeat")
	serve("HeartbeatTestReqID")
	serve("TestRequest")
	if hb := decode[fixsbe.Heartbeat](t, p.expect(t, fixsbe.HeartbeatTemplateID)); string(hb.TestReqID) != "tr-1" {
		t.Errorf("TestRequest answered with %q", hb.TestReqID)
	}
	for _, name := range []string{"Reject", "RejectNulls"} {
		want := serve(name)
		if e := h.next(t); e.kind != "Reject" || e.err != nil || !bytes.Equal(e.b, want) || e.in.Receipt != nil {
			t.Errorf("%s: %s %v %v\n%x\nwant\n%x", name, e.kind, e.err, e.in.Receipt, e.b, want)
		}
	}
	for _, tc := range []struct {
		name      string
		remaining time.Duration
	}{{"News", 30 * time.Second}, {"NewsLong", -1}} {
		want := serve(tc.name)
		var n fixsbe.News
		if _, err := n.Decode(want); err != nil {
			t.Fatal(err)
		}
		if e := h.next(t); e.kind != "news" || e.ev.(NewsEvent).Headline != string(n.Headline) || e.ev.(NewsEvent).Remaining != tc.remaining {
			t.Errorf("%s: %+v", tc.name, e)
		}
	}

	// Requests decoded from Real Logic's bytes, sent: the same bytes.
	receipts := map[string]*Receipt{}
	for _, tc := range []struct {
		name string
		m    interface {
			Request
			Decode([]byte) (int, error)
		}
	}{
		{"NewOrderSingle", new(NewOrderSingle)},
		{"OrderCancelRequest", new(OrderCancelRequest)},
		{"OrderCancelRequestAndNewOrderSingle", new(OrderCancelRequestAndNewOrderSingle)},
		{"OrderAmendKeepPriorityRequest", new(OrderAmendKeepPriorityRequest)},
		{"OrderMassCancelRequest", new(OrderMassCancelRequest)},
		{"LimitQuery", new(LimitQuery)},
		{"NewOrderList", new(NewOrderList)},
	} {
		want := frames[tc.name][lesofh.HeaderLen:]
		if _, err := tc.m.Decode(want); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		r, err := c.Send(tc.m)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		receipts[tc.name] = r
		m := p.next(t)
		wh, _, _ := fixsbe.ReadHeader(want)
		if m.h.BlockLength != wh.BlockLength || m.h.TemplateId != wh.TemplateId || m.h.SchemaId != wh.SchemaId ||
			m.h.Version != wh.Version || !bytes.Equal(m.body, want[fixsbe.HeaderSize:]) {
			t.Errorf("%s: header %+v, body\n%x\nReal Logic: header %+v, body\n%x", tc.name, m.h, m.body, wh, want[fixsbe.HeaderSize:])
		}
		waitState(t, r, Written)
	}

	// Their answers, and messages answering nothing of ours.
	for _, tc := range []struct {
		name, kind, receipt string
	}{
		{"ExecutionReport", "ExecutionReport", "NewOrderSingle"},
		{"ExecutionReportNulls", "ExecutionReport", ""},
		{"ExecutionReportAck", "ExecutionReportAck", "OrderCancelRequestAndNewOrderSingle"},
		{"ExecutionReportAckRejected", "ExecutionReportAck", ""},
		{"OrderCancelReject", "OrderCancelReject", "OrderCancelRequest"},
		{"OrderAmendReject", "OrderAmendReject", "OrderAmendKeepPriorityRequest"},
		{"ListStatus", "ListStatus", "NewOrderList"},
		{"ListStatusEmpty", "ListStatus", ""},
		{"OrderMassCancelReport", "OrderMassCancelReport", "OrderMassCancelRequest"},
		{"OrderMassCancelReportRejected", "OrderMassCancelReport", ""},
		{"LimitResponse", "LimitResponse", "LimitQuery"},
		{"LimitResponseEmpty", "LimitResponse", ""},
		// Market data: not an order entry message.
		{"MarketDataIncrementalTrade", "unknown", ""},
	} {
		want := serve(tc.name)
		e := h.next(t)
		if tc.kind == "unknown" {
			want = want[fixsbe.HeaderSize:]
		}
		if e.kind != tc.kind || e.err != nil || !bytes.Equal(e.b, want) {
			t.Errorf("%s: %s, %v\n%x\nwant\n%x", tc.name, e.kind, e.err, e.b, want)
		}
		if e.in.Receipt != receipts[tc.receipt] {
			t.Errorf("%s: receipt %v, want %v", tc.name, e.in.Receipt, receipts[tc.receipt])
		}
	}
	for name, r := range receipts {
		want := Answered
		if name == "OrderCancelRequestAndNewOrderSingle" {
			want = Written // its cancellation unanswered
		}
		if r.State() != want {
			t.Errorf("%s: %v, want %v; legs %s", name, r.State(), want, legs(r))
		}
	}
	if s := legs(receipts["NewOrderList"]); s != "list nol-1 accepted;order nol-a accepted;order nol-b accepted;order nol-c refused;" {
		t.Errorf("NewOrderList legs %s", s)
	}
	if s := legs(receipts["OrderCancelRequestAndNewOrderSingle"]); s != "cancel xcn-c pending;order xcn-n accepted;" {
		t.Errorf("OrderCancelRequestAndNewOrderSingle legs %s", s)
	}

	// Binance's Logout, answered with ours; the request awaiting its
	// cancellation's answer PossiblyTransmitted.
	serve("Logout")
	p.expect(t, fixsbe.LogoutTemplateID)
	e := h.next(t)
	le, _ := e.ev.(LogoutEvent)
	var pl *PeerLogoutError
	if e.kind != "logout" || !errors.As(le.Err, &pl) || pl.Text != "Logout acknowledgment." || !le.Clean ||
		describe(le.PossiblyTransmitted) != describe([]*Receipt{receipts["OrderCancelRequestAndNewOrderSingle"]}) || len(le.NeverWritten) != 0 {
		t.Errorf("LogoutEvent %+v", e)
	}
	if err := runErr(t, done); !errors.As(err, &pl) {
		t.Errorf("Run: %v", err)
	}
}
