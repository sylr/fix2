package binancefix

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/transport"
)

// reportOf returns an ExecutionReport of ClOrdID id and ExecType et.
func reportOf(id string, et fixsbe.ExecType) *ExecutionReport {
	er := report(id)
	er.ExecType = et
	switch et {
	case fixsbe.ExecTypeCanceled:
		er.OrdStatus = fixsbe.OrdStatusCanceled
	case fixsbe.ExecTypeRejected:
		er.OrdStatus = fixsbe.OrdStatusRejected
	case fixsbe.ExecTypeTrade:
		er.OrdStatus = fixsbe.OrdStatusPartiallyFilled
	}
	return er
}

// session starts a Client logged on to a peer.
func session(t *testing.T, cfg func(*Config)) (*Client, *peer, *recorder) {
	t.Helper()
	f, h := newFake(t), newRecorder()
	c0 := testConfig(f, h)
	if cfg != nil {
		cfg(&c0)
	}
	c, _ := start(t, c0, time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	return c, p, h
}

// legs describes r's legs.
func legs(r *Receipt) string {
	var b bytes.Buffer
	for _, l := range r.Legs() {
		fmt.Fprintf(&b, "%s %s %s;", l.Kind, l.ID, l.Outcome)
	}
	return b.String()
}

// An amendment may carry its order's ClOrdID: that order's fills do not
// answer it, its ExecutionReport REPLACED or OrderAmendReject does; left
// unanswered, it is PossiblyTransmitted.
func TestAmendment(t *testing.T) {
	c, p, h := session(t, nil)
	o, _ := c.Send(order("o1"))
	p.expect(t, fixsbe.NewOrderSingleTemplateID)
	p.send(t, report("o1"))
	h.next(t, "er")
	waitState(t, o, Answered)

	amend := func(id string) *Receipt {
		r, err := c.Send(&OrderAmendKeepPriorityRequest{OrderID: Some[int64](1), QtyExponent: -8, OrderQty: 5, ClOrdID: []byte(id),
			OrigClOrdID: []byte("o1"), Symbol: []byte("BTCUSDT")})
		if err != nil {
			t.Fatal(err)
		}
		p.expect(t, fixsbe.OrderAmendKeepPriorityRequestTemplateID)
		return r
	}
	a := amend("o1")
	p.send(t, reportOf("o1", fixsbe.ExecTypeTrade))
	if r := h.next(t, "er"); r.in.Receipt != nil {
		t.Errorf("a fill answered the amendment")
	}
	p.send(t, reportOf("o1", fixsbe.ExecTypeReplaced))
	if r := h.next(t, "er"); r.in.Receipt != a || a.State() != Answered {
		t.Errorf("REPLACED: %v, %s", a.State(), legs(a))
	}
	b := amend("o1")
	p.send(t, &fixsbe.OrderAmendReject{QtyExponent: -8, OrderQty: 5, ErrorCode: -2038, ClOrdID: []byte("o1"), OrigClOrdID: []byte("o1"),
		Symbol: []byte("BTCUSDT"), ErrorText: []byte("no change")})
	waitState(t, b, Answered)
	if l := b.Legs()[0]; l.Outcome != Refused || l.Answer.ErrorCode != Some[int32](-2038) {
		t.Errorf("OrderAmendReject: %+v", l)
	}

	// A fill, then the connection ends before the answer.
	d := amend("o1")
	p.send(t, reportOf("o1", fixsbe.ExecTypeTrade))
	h.next(t, "er")
	p.c.Close()
	e := h.next(t, "logout").ev.(LogoutEvent)
	if d.State() != PossiblyTransmitted || len(e.PossiblyTransmitted) != 1 || e.PossiblyTransmitted[0] != d {
		t.Errorf("amendment %v; event %s", d.State(), describe(e.PossiblyTransmitted))
	}
}

func cancelReplace(mode fixsbe.OrderCancelRequestAndNewOrderSingleMode) *OrderCancelRequestAndNewOrderSingle {
	return &OrderCancelRequestAndNewOrderSingle{OrderCancelRequestAndNewOrderSingleMode: mode, OrderID: Some[int64](1),
		PriceExponent: -2, QtyExponent: -8, OrderQty: Some[int64](1), OrdType: fixsbe.OrdTypeLimit, Price: Some[int64](100),
		Side: fixsbe.SideBuy, TimeInForce: Some(fixsbe.TimeInForceGoodTillCancel),
		CancelClOrdID: []byte("c1"), OrigClOrdID: []byte{}, ClOrdID: []byte("n1"), Symbol: []byte("BTCUSDT")}
}

// A cancel/replace is answered once both its cancellation and its new
// order are: the cancellation alone leaves the new order unknown.
func TestCancelReplace(t *testing.T) {
	allow, stop := fixsbe.OrderCancelRequestAndNewOrderSingleModeAllowFailure, fixsbe.OrderCancelRequestAndNewOrderSingleModeStopOnFailure
	cancelReject := func(id string) *fixsbe.OrderCancelReject {
		return &fixsbe.OrderCancelReject{CxlRejResponseTo: fixsbe.CxlRejResponseToOrderCancelRequest, ErrorCode: -2011,
			ClOrdID: []byte(id), OrigClOrdID: []byte{}, OrigClListID: []byte{}, Symbol: []byte("BTCUSDT"), ErrorText: []byte("Unknown order sent.")}
	}
	for _, tc := range []struct {
		name    string
		mode    fixsbe.OrderCancelRequestAndNewOrderSingleMode
		answers []encoder
		state   State
		legs    string
	}{
		{"cancelled, new order unanswered", allow, []encoder{reportOf("c1", fixsbe.ExecTypeCanceled)}, PossiblyTransmitted,
			"cancel c1 accepted;order n1 pending;"},
		{"cancelled, new order placed", allow, []encoder{reportOf("c1", fixsbe.ExecTypeCanceled), reportOf("n1", fixsbe.ExecTypeNew)}, Answered,
			"cancel c1 accepted;order n1 accepted;"},
		{"cancelled, new order rejected", allow, []encoder{reportOf("c1", fixsbe.ExecTypeCanceled), reportOf("n1", fixsbe.ExecTypeRejected)}, Answered,
			"cancel c1 accepted;order n1 refused;"},
		{"cancel refused, stop on failure", stop, []encoder{cancelReject("c1")}, Answered,
			"cancel c1 refused;order n1 refused;"},
		{"cancel refused by the request's ClOrdID, stop on failure", stop, []encoder{cancelReject("n1")}, Answered,
			"cancel c1 refused;order n1 refused;"},
		{"cancel refused, allow failure, new order unanswered", allow, []encoder{cancelReject("c1")}, PossiblyTransmitted,
			"cancel c1 refused;order n1 pending;"},
		{"cancel refused, allow failure, new order placed", allow, []encoder{cancelReject("c1"), reportOf("n1", fixsbe.ExecTypeNew)}, Answered,
			"cancel c1 refused;order n1 accepted;"},
		// As the testnet answered on 2026-10-17: the new order's NEW
		// before the OrderCancelReject, each leg answered on its own.
		{"cancel refused, allow failure, new order placed first", allow, []encoder{reportOf("n1", fixsbe.ExecTypeNew), cancelReject("c1")}, Answered,
			"cancel c1 refused;order n1 accepted;"},
		// As the testnet answered: the OrderCancelReject, then the new
		// order's REJECTED -2010 "order placement not attempted", which
		// finds the request already answered.
		{"cancel refused, stop on failure, then not attempted", stop, []encoder{cancelReject("c1"), notAttempted("n1")}, Answered,
			"cancel c1 refused;order n1 refused;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, h := session(t, nil)
			r, err := c.Send(cancelReplace(tc.mode))
			if err != nil {
				t.Fatal(err)
			}
			p.expect(t, fixsbe.OrderCancelRequestAndNewOrderSingleTemplateID)
			for _, a := range tc.answers {
				p.send(t, a)
				kind := "cancelreject"
				if isER(a) {
					kind = "er"
				}
				h.next(t, kind)
			}
			if tc.state == PossiblyTransmitted {
				p.c.Close()
				h.next(t, "logout")
			}
			waitState(t, r, tc.state)
			if got := legs(r); got != tc.legs {
				t.Errorf("legs %s, want %s", got, tc.legs)
			}
		})
	}
	// Without CancelClOrdID, the cancellation's answer could not be told.
	c, err := New(testConfig(newFake(t), nil))
	if err != nil {
		t.Fatal(err)
	}
	m := cancelReplace(allow)
	m.CancelClOrdID = []byte{}
	if _, err := c.Send(m); !errors.Is(err, ErrInvalidID) {
		t.Errorf("Send without CancelClOrdID: %v", err)
	}
}

func isER(e encoder) bool { _, ok := e.(*ExecutionReport); return ok }

// notAttempted is the ExecutionReport REJECTED the testnet sends for the
// new order of a STOP_ON_FAILURE cancel/replace whose cancellation failed.
func notAttempted(id string) *ExecutionReport {
	er := reportOf(id, fixsbe.ExecTypeRejected)
	er.OrderID, er.ErrorCode, er.ErrorText = Optional[int64]{}, Some[int32](-2010), []byte("Unable to cancel order, order placement not attempted.")
	return er
}

// The cancellation of a list, as the testnet answered it on 2026-10-17: a
// ListStatus ALL_DONE carrying the cancellation's ClOrdID as ClListID
// first, which does not answer it, then an ExecutionReport CANCELED per
// order carrying the cancellation's ClOrdID, the first of which does.
func TestListCancelAnsweredAfterListStatus(t *testing.T) {
	c, p, h := session(t, nil)
	r, err := c.Send(&OrderCancelRequest{ClOrdID: []byte("x"), OrigClOrdID: []byte{}, OrigClListID: []byte("L"), Symbol: []byte("BTCUSDT")})
	if err != nil {
		t.Fatal(err)
	}
	p.expect(t, fixsbe.OrderCancelRequestTemplateID)
	p.send(t, &fixsbe.ListStatus{ListID: Some[int64](9), ListStatusType: fixsbe.ListStatusTypeAllDone, ListOrderStatus: fixsbe.ListOrderStatusAllDone,
		ClListID: []byte("x"), OrigClListID: []byte("L"),
		Orders: []ListStatusOrders{{OrderID: Some[int64](91), ClOrdID: []byte("w"), Symbol: []byte("BTCUSDT")}, {OrderID: Some[int64](92), ClOrdID: []byte("p"), Symbol: []byte("BTCUSDT")}}})
	if x := h.next(t, "list"); x.in.Receipt != nil || r.State() != Written {
		t.Fatalf("the ListStatus answered the cancellation: %v, %s", r.State(), legs(r))
	}
	cancelled := func(orig string) *ExecutionReport {
		er := reportOf("x", fixsbe.ExecTypeCanceled)
		er.OrigClOrdID, er.ListID = []byte(orig), Some[int64](9)
		return er
	}
	p.send(t, cancelled("w"))
	if x := h.next(t, "er"); x.in.Receipt != r {
		t.Fatalf("the first CANCELED did not answer the cancellation")
	}
	waitState(t, r, Answered)
	if got := legs(r); got != "cancel x accepted;" {
		t.Errorf("legs %s", got)
	}
	p.send(t, cancelled("p"))
	if x := h.next(t, "er"); x.in.Receipt != nil {
		t.Errorf("the second CANCELED matched a request")
	}
}

// A ListStatus whose order entry carries -1007 leaves that order's status
// unknown: the list is StatusUnknown, not Answered.
func TestListStatusTimeout(t *testing.T) {
	c, p, h := session(t, nil)
	r, err := c.Send(&NewOrderList{ContingencyType: fixsbe.ContingencyTypeOneCancelsTheOther, ClListID: []byte("L2"),
		Orders: []NewOrderListOrders{{OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideSell, ClOrdID: []byte("a2"), Symbol: []byte("BTCUSDT")},
			{OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideSell, ClOrdID: []byte("b2"), Symbol: []byte("BTCUSDT")}}})
	if err != nil {
		t.Fatal(err)
	}
	p.expect(t, fixsbe.NewOrderListTemplateID)
	p.send(t, &fixsbe.ListStatus{ListStatusType: fixsbe.ListStatusTypeResponse, ListOrderStatus: fixsbe.ListOrderStatusExecuting,
		ClListID: []byte("L2"), OrigClListID: []byte{},
		Orders: []ListStatusOrders{{OrderID: Some[int64](21), ClOrdID: []byte("a2"), Symbol: []byte("BTCUSDT"), ErrorText: []byte{}},
			{ErrorCode: Some[int32](-1007), ClOrdID: []byte("b2"), Symbol: []byte("BTCUSDT"), ErrorText: []byte("Timeout")}}})
	if x := h.next(t, "list"); x.in.Receipt != r {
		t.Errorf("ListStatus not matched")
	}
	waitState(t, r, StatusUnknown)
	if got, want := legs(r), "list L2 unknown;order a2 accepted;order b2 unknown;"; got != want {
		t.Errorf("legs %s, want %s", got, want)
	}
	if a, _ := r.Answer(); a.ErrorCode != Some[int32](-1007) {
		t.Errorf("Answer %+v", a)
	}
	// Each order's answer carries its own error code, the list's the
	// aggregate.
	checkCodes(t, r, []Optional[int32]{Some[int32](-1007), {}, Some[int32](-1007)})

	// Mixed errors: one refused, one timed out, one placed.
	r, err = c.Send(&NewOrderList{ContingencyType: fixsbe.ContingencyTypeOneCancelsTheOther, ClListID: []byte("L4"),
		Orders: []NewOrderListOrders{{OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideSell, ClOrdID: []byte("a4"), Symbol: []byte("BTCUSDT")},
			{OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideSell, ClOrdID: []byte("b4"), Symbol: []byte("BTCUSDT")},
			{OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideSell, ClOrdID: []byte("c4"), Symbol: []byte("BTCUSDT")}}})
	if err != nil {
		t.Fatal(err)
	}
	p.expect(t, fixsbe.NewOrderListTemplateID)
	p.send(t, &fixsbe.ListStatus{ListStatusType: fixsbe.ListStatusTypeResponse, ListOrderStatus: fixsbe.ListOrderStatusExecuting,
		ClListID: []byte("L4"), OrigClListID: []byte{},
		Orders: []ListStatusOrders{{ErrorCode: Some[int32](-2010), ClOrdID: []byte("a4"), Symbol: []byte("BTCUSDT"), ErrorText: []byte("insufficient balance")},
			{ErrorCode: Some[int32](-1007), ClOrdID: []byte("b4"), Symbol: []byte("BTCUSDT"), ErrorText: []byte("Timeout")},
			{OrderID: Some[int64](43), ClOrdID: []byte("c4"), Symbol: []byte("BTCUSDT"), ErrorText: []byte{}}}})
	h.next(t, "list")
	waitState(t, r, StatusUnknown)
	if got, want := legs(r), "list L4 unknown;order a4 refused;order b4 unknown;order c4 accepted;"; got != want {
		t.Errorf("legs %s, want %s", got, want)
	}
	checkCodes(t, r, []Optional[int32]{Some[int32](-1007), Some[int32](-2010), Some[int32](-1007), {}})
}

// checkCodes checks the error codes of r's legs' answers.
func checkCodes(t *testing.T, r *Receipt, want []Optional[int32]) {
	t.Helper()
	for i, l := range r.Legs() {
		if l.Answer.ErrorCode != want[i] || l.Answer.TemplateID != fixsbe.ListStatusTemplateID {
			t.Errorf("leg %s: answer %+v, want error code %+v", l.ID, l.Answer, want[i])
		}
	}
}

// ClListID is optional in a ListStatus: entries naming our list's orders
// answer them without it.
func TestListStatusWithoutClListID(t *testing.T) {
	c, p, h := session(t, nil)
	r, err := c.Send(&NewOrderList{ContingencyType: fixsbe.ContingencyTypeOneCancelsTheOther, ClListID: []byte("L3"),
		Orders: []NewOrderListOrders{{OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideSell, ClOrdID: []byte("a3"), Symbol: []byte("BTCUSDT")},
			{OrdType: fixsbe.OrdTypeLimit, Side: fixsbe.SideSell, ClOrdID: []byte("b3"), Symbol: []byte("BTCUSDT")}}})
	if err != nil {
		t.Fatal(err)
	}
	p.expect(t, fixsbe.NewOrderListTemplateID)
	p.send(t, &fixsbe.ListStatus{ListStatusType: fixsbe.ListStatusTypeResponse, ListOrderStatus: fixsbe.ListOrderStatusExecuting,
		ClListID: []byte("L3"), OrigClListID: []byte{}})
	if x := h.next(t, "list"); x.in.Receipt != r || r.State() != Written {
		t.Errorf("acknowledgement: %v, %s", r.State(), legs(r))
	}
	p.send(t, &fixsbe.ListStatus{ListStatusType: fixsbe.ListStatusTypeExecStarted, ListOrderStatus: fixsbe.ListOrderStatusExecuting,
		ClListID: []byte{}, OrigClListID: []byte{},
		Orders: []ListStatusOrders{{OrderID: Some[int64](31), ClOrdID: []byte("a3"), Symbol: []byte("BTCUSDT"), ErrorText: []byte{}},
			{OrderID: Some[int64](32), ClOrdID: []byte("b3"), Symbol: []byte("BTCUSDT"), ErrorText: []byte{}}}})
	if x := h.next(t, "list"); x.in.Receipt != r {
		t.Errorf("entries without ClListID not matched")
	}
	waitState(t, r, Answered)
	if got, want := legs(r), "list L3 accepted;order a3 accepted;order b3 accepted;"; got != want {
		t.Errorf("legs %s, want %s", got, want)
	}
}

// A ListStatus of the list, an update after a fill, does not answer the
// list's cancellation: left unanswered, it is PossiblyTransmitted.
func TestListCancelNotAnsweredByUpdate(t *testing.T) {
	c, p, h := session(t, nil)
	r, err := c.Send(&OrderCancelRequest{ClOrdID: []byte("L"), OrigClOrdID: []byte{}, OrigClListID: []byte("L"), Symbol: []byte("BTCUSDT")})
	if err != nil {
		t.Fatal(err)
	}
	p.expect(t, fixsbe.OrderCancelRequestTemplateID)
	p.send(t, &fixsbe.ListStatus{ListID: Some[int64](9), ListStatusType: fixsbe.ListStatusTypeUpdated, ListOrderStatus: fixsbe.ListOrderStatusExecuting,
		ClListID: []byte("L"), OrigClListID: []byte("L"),
		Orders: []ListStatusOrders{{OrderID: Some[int64](91), ClOrdID: []byte("w"), Symbol: []byte("BTCUSDT"), ErrorText: []byte{}}}})
	if x := h.next(t, "list"); x.in.Receipt != nil {
		t.Errorf("an update answered the cancellation: %s", legs(r))
	}
	p.c.Close()
	h.next(t, "logout")
	if r.State() != PossiblyTransmitted {
		t.Errorf("cancellation %v, %s", r.State(), legs(r))
	}
}

// A TestRequest's write returning late does not arm the timer of a newer
// one, answered meanwhile and replaced, not yet written.
func TestTestRequestWriteGeneration(t *testing.T) {
	const interval = 100 * time.Millisecond
	f, h := newFake(t), newRecorder()
	var g *gate
	f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
	c, _ := start(t, testConfig(f, h), interval)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	steps := make(chan int)
	g.mu.Lock()
	g.steps = steps
	g.mu.Unlock()
	// Writes go through until one carries our TestRequest A, transmitted
	// but its Write held.
	var a []byte
	for a == nil {
		g.waitWrite(t)
		steps <- -2
		for drained := false; !drained; {
			select {
			case m := <-p.msgs:
				if m.h.TemplateId == fixsbe.TestRequestTemplateID {
					a = bytes.Clone(decode[fixsbe.TestRequest](t, m).TestReqID)
				}
			case <-time.After(interval / 5):
				drained = true
			}
		}
		if a == nil {
			steps <- 0 // the write returns
		}
	}
	// A answered; silence then queues B.
	p.send(t, &fixsbe.Heartbeat{TestReqID: a})
	time.Sleep(interval * 3 / 2)
	steps <- 0 // A's write returns
	// B's write waits before transmission, longer than HeartBtInt.
	g.waitWrite(t)
	time.Sleep(2 * interval)
	if !c.LoggedOn() {
		t.Fatal("ended while our TestRequest waited to be written")
	}
	steps <- -1
	for {
		m := p.next(t)
		if m.h.TemplateId == fixsbe.TestRequestTemplateID {
			p.send(t, &fixsbe.Heartbeat{TestReqID: bytes.Clone(decode[fixsbe.TestRequest](t, m).TestReqID)})
			break
		}
	}
	g.mu.Lock()
	g.steps = nil
	g.mu.Unlock()
}

// Our TestRequest's answer is awaited from when it is written, not from
// when it waited behind a blocked write.
func TestTestRequestTimerFromWrite(t *testing.T) {
	const interval = 100 * time.Millisecond
	f, h := newFake(t), newRecorder()
	var g *gate
	f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
	c, _ := start(t, testConfig(f, h), interval)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	g.shut()
	if _, err := c.Send(order("o1")); err != nil {
		t.Fatal(err)
	}
	g.waitWrite(t)
	time.Sleep(3 * interval) // the TestRequest is queued meanwhile
	g.release(0)
	until := time.Now().Add(2 * interval)
	tests := 0
	for time.Now().Before(until) {
		select {
		case m := <-p.msgs:
			switch m.h.TemplateId {
			case fixsbe.LogoutTemplateID:
				t.Fatalf("Logout: %q", decode[fixsbe.Logout](t, m).Text)
			case fixsbe.TestRequestTemplateID:
				tests++
				p.send(t, &fixsbe.Heartbeat{TestReqID: bytes.Clone(decode[fixsbe.TestRequest](t, m).TestReqID)})
			}
		case <-time.After(interval / 4):
		}
	}
	if tests == 0 || !c.LoggedOn() {
		t.Errorf("TestRequests %d, logged on %v", tests, c.LoggedOn())
	}
}

// Binance's TestRequests while our writer is stuck: their answers are not
// queued without bound, the connection ends.
func TestAnswerBacklog(t *testing.T) {
	f, h := newFake(t), newRecorder()
	var g *gate
	f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
	c, _ := start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	g.shut()
	if _, err := c.Send(order("o1")); err != nil {
		t.Fatal(err)
	}
	g.waitWrite(t)
	for i := range 2 * maxAnswers {
		p.send(t, &fixsbe.TestRequest{TestReqID: []byte(fmt.Sprint("t", i))})
	}
	if e := h.next(t, "logout").ev.(LogoutEvent); !errors.Is(e.Err, ErrBacklog) {
		t.Errorf("LogoutEvent %+v", e)
	}
}

// IDs returns a copy: changing it changes neither matching nor which IDs
// are free again once the request is answered.
func TestReceiptIDsCopy(t *testing.T) {
	c, p, h := session(t, nil)
	r, err := c.Send(order("a"))
	if err != nil {
		t.Fatal(err)
	}
	r.IDs()[0] = "zzz"
	r.Legs()[0].ID = "zzz"
	p.expect(t, fixsbe.NewOrderSingleTemplateID)
	p.send(t, report("a"))
	h.next(t, "er")
	waitState(t, r, Answered)
	if r.IDs()[0] != "a" || r.Legs()[0].ID != "a" {
		t.Errorf("IDs %v", r.IDs())
	}
	if _, err := c.Send(order("a")); err != nil {
		t.Errorf("Send of an answered request's ClOrdID: %v", err)
	}
}
