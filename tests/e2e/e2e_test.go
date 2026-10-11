package e2e

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"syscall"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
	"github.com/sylr/fix2/session/transport/tcp"
)

const (
	// orders is how many NewOrderSingle the initiator sends, fills how many
	// trades fill each.
	orders = 50
	fills  = 3
	// timeout bounds every wait.
	timeout = 10 * time.Second
)

// transports connect the initiator to the acceptor.
var transports = []struct {
	name   string
	listen func(t *testing.T) (transport.Listener, transport.Dialer)
}{
	{"tcp", func(t *testing.T) (transport.Listener, transport.Dialer) {
		l, err := tcp.Listen("127.0.0.1:0", nil)
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("cannot listen in this environment: %v", err)
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		return l, &tcp.Dialer{Addresses: []string{l.Addr().String()}}
	}},
	{"mem", func(t *testing.T) (transport.Listener, transport.Dialer) {
		l := mem.Listen(t.Name())
		t.Cleanup(func() { l.Close() })
		return l, l
	}},
}

// peer is what the handlers of both ends share: it tells of logons and
// logouts, and fails the test on any Reject (35=3), which a session sends
// for a message its dictionaries do not validate.
type peer struct {
	session.BaseHandler
	t       *testing.T
	d       dialect
	logons  chan struct{}
	logouts chan error
}

func newPeer(t *testing.T, d dialect) peer {
	return peer{t: t, d: d, logons: make(chan struct{}, 8), logouts: make(chan error, 8)}
}

func (p *peer) OnLogon(context.Context, *session.Session) {
	select {
	case p.logons <- struct{}{}:
	default:
	}
}

func (p *peer) OnLogout(_ context.Context, _ *session.Session, reason error) {
	select {
	case p.logouts <- reason:
	default:
	}
}

func (p *peer) FromAdmin(_ context.Context, s *session.Session, m *fix2.Message, _ session.Inbound) {
	if string(m.MsgType()) == "3" {
		p.t.Errorf("%s received a Reject: RefSeqNum %s, %s", s.ID(), m.Get(45), m.Get(58))
	}
}

// unexpected fails the test on an application message of an unexpected
// type, a BusinessMessageReject (35=j) for one, which it does not reject:
// the two ends would reject each other's rejects forever.
func (p *peer) unexpected(s *session.Session, m *fix2.Message) error {
	p.t.Errorf("%s received MsgType %s: %s", s.ID(), m.MsgType(), m.Get(58))
	if string(m.MsgType()) == "j" {
		return nil
	}
	return fix2.ErrUnsupportedMessageType
}

// exchange is the acceptor's application: it acknowledges each order with an
// ExecutionReport, then fills it in trades of equal size, the last taking
// the rest, at its limit price.
type exchange struct {
	peer
	next int // OrderID of the next order: FromApp runs on one goroutine

	mu       sync.Mutex
	received map[string]order    // by ClOrdID
	sent     map[string][]report // by ClOrdID
}

func (x *exchange) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, _ session.Inbound) error {
	if string(m.MsgType()) != "D" {
		return x.unexpected(s, m)
	}
	o, err := x.d.readOrder(m)
	if err != nil {
		x.t.Errorf("exchange: NewOrderSingle: %v", err)
		return err
	}
	x.next++
	reports := execute(o, fmt.Sprintf("ORDER-%d", x.next))
	x.mu.Lock()
	x.received[o.ClOrdID] = o
	x.sent[o.ClOrdID] = reports
	x.mu.Unlock()
	for _, r := range reports {
		// A Send from a callback never waits: it fails if the queue is full,
		// which these few reports do not fill.
		if _, err := s.Send(ctx, x.d.newReport(r)); err != nil {
			x.t.Errorf("exchange: sending ExecutionReport %s: %v", r.ExecID, err)
			return err
		}
	}
	return nil
}

// execute returns the ExecutionReports of order o: its acknowledgement, then
// its fills.
func execute(o order, orderID string) []report {
	now := time.Now().UTC().Truncate(time.Millisecond)
	r := report{
		OrderID: orderID, ExecID: orderID + "-0", ClOrdID: o.ClOrdID,
		ExecType: execNew, OrdStatus: statusNew,
		Symbol: o.Symbol, Side: o.Side, OrderQty: o.Qty, Price: o.Price,
		LeavesQty: o.Qty, TransactTime: now,
	}
	reports := []report{r}
	lot := math.Floor(o.Qty / fills)
	for i := 1; i <= fills; i++ {
		qty := lot
		if i == fills {
			qty = o.Qty - r.CumQty
		}
		r.ExecID = fmt.Sprintf("%s-%d", orderID, i)
		r.ExecType, r.OrdStatus = execTrade, statusPartial
		r.LastQty, r.LastPx = qty, o.Price
		r.CumQty += qty
		r.LeavesQty = o.Qty - r.CumQty
		r.AvgPx = o.Price
		if r.LeavesQty == 0 {
			r.OrdStatus = statusFilled
		}
		reports = append(reports, r)
	}
	return reports
}

// client is the initiator's application: it passes on the ExecutionReports
// it receives.
type client struct {
	peer
	reports chan report
}

func (c *client) FromApp(_ context.Context, s *session.Session, m *fix2.Message, _ session.Inbound) error {
	if string(m.MsgType()) != "8" {
		return c.unexpected(s, m)
	}
	r, err := c.d.readReport(m)
	if err != nil {
		c.t.Errorf("client: ExecutionReport: %v", err)
		return err
	}
	select {
	case c.reports <- r:
	default:
		c.t.Errorf("client: ExecutionReport %s for %s beyond those expected", r.ExecID, r.ClOrdID)
	}
	return nil
}

// TestOrderFlow logs an initiator on to an acceptor, in FIX 4.4 and in FIX
// 5.0 SP2 over FIXT 1.1, over TCP and in memory. The initiator sends orders;
// the acceptor acknowledges and fills each; the initiator checks every
// ExecutionReport is the one sent, and each order's go through its life:
// new, partially filled, filled. Then the initiator logs out.
func TestOrderFlow(t *testing.T) {
	for _, d := range dialects {
		for _, tr := range transports {
			t.Run(d.name+"/"+tr.name, func(t *testing.T) {
				t.Parallel()
				l, dialer := tr.listen(t)
				testOrderFlow(t, d, l, dialer)
			})
		}
	}
}

func testOrderFlow(t *testing.T, d dialect, l transport.Listener, dialer transport.Dialer) {
	x := &exchange{peer: newPeer(t, d), received: map[string]order{}, sent: map[string][]report{}}
	c := &client{peer: newPeer(t, d), reports: make(chan report, orders*(fills+1))}

	acceptor := session.NewAcceptor(session.AcceptorOptions{})
	broker, err := acceptor.Add(session.Config{
		ID:           session.SessionID{BeginString: d.beginString, SenderCompID: "BROKER", TargetCompID: "CLIENT"},
		Dictionaries: d.dictionaries,
		Handler:      x,
		// Under load, the counterparty's Logout may come late: waiting for
		// it keeps the sequence numbers of both ends in step.
		LogoutTimeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	acceptor.Listen(l)
	initiator := session.NewInitiator(session.InitiatorOptions{})
	trader, err := initiator.Add(session.Config{
		ID:            session.SessionID{BeginString: d.beginString, SenderCompID: "CLIENT", TargetCompID: "BROKER"},
		Dictionaries:  d.dictionaries,
		Handler:       c,
		LogoutTimeout: timeout,
	}, dialer)
	if err != nil {
		t.Fatal(err)
	}

	// The initiator runs until it logs out, the acceptor until the test
	// ends.
	actx, acancel := context.WithCancel(context.Background())
	ictx, icancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := acceptor.Run(actx); err != nil {
			t.Errorf("acceptor: Run: %v", err)
		}
	})
	wg.Go(func() {
		if err := initiator.Run(ictx); err != nil {
			t.Errorf("initiator: Run: %v", err)
		}
	})
	t.Cleanup(func() { icancel(); acancel(); wg.Wait() })

	wait(t, "initiator's logon", c.logons)
	wait(t, "acceptor's logon", x.logons)

	symbols := []string{"AAPL", "MSFT", "GOOG", "AMZN"}
	sent := make([]order, orders)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for i := range sent {
		o := order{
			ClOrdID:      fmt.Sprintf("CL-%d", i+1),
			Symbol:       symbols[i%len(symbols)],
			Side:         [2]string{buy, sell}[i%2],
			Qty:          float64(100 * (i%5 + 1)),
			Price:        100 + 0.25*float64(i),
			TransactTime: time.Now().UTC().Truncate(time.Millisecond),
		}
		sent[i] = o
		if _, err := trader.Send(ctx, d.newOrder(o)); err != nil {
			t.Fatalf("sending order %s: %v", o.ClOrdID, err)
		}
	}

	received := map[string][]report{}
	for range orders * (fills + 1) {
		select {
		case r := <-c.reports:
			received[r.ClOrdID] = append(received[r.ClOrdID], r)
		case <-ctx.Done():
			// A FIXT session's Rejects of messages its dictionaries do not
			// define reach no handler: say how many were sent.
			t.Fatalf("received %d ExecutionReports of %d; Rejects sent: %d by the initiator, %d by the acceptor",
				countReports(received), orders*(fills+1), trader.Stats().RejectsSent.Load(), broker.Stats().RejectsSent.Load())
		}
	}

	orderIDs := map[string]string{} // ClOrdID by OrderID
	execIDs := map[string]bool{}
	x.mu.Lock()
	accepted, acked := x.received, x.sent
	x.mu.Unlock()
	for _, o := range sent {
		if got := accepted[o.ClOrdID]; !got.equal(o) {
			t.Errorf("order %s: the acceptor received %+v, want %+v", o.ClOrdID, got, o)
		}
		got := received[o.ClOrdID]
		checkLife(t, o, got)
		if want := acked[o.ClOrdID]; !sameReports(got, want) {
			t.Errorf("order %s: received %+v, the acceptor sent %+v", o.ClOrdID, got, want)
		}
		for _, r := range got {
			if other, ok := orderIDs[r.OrderID]; ok && other != o.ClOrdID {
				t.Errorf("orders %s and %s share OrderID %s", other, o.ClOrdID, r.OrderID)
			}
			orderIDs[r.OrderID] = o.ClOrdID
			if execIDs[r.ExecID] {
				t.Errorf("ExecID %s reported twice", r.ExecID)
			}
			execIDs[r.ExecID] = true
		}
	}

	// Stopping the initiator logs it out: a logout told before is that of
	// an earlier connection, which should not have ended.
	for _, p := range []*peer{&x.peer, &c.peer} {
		select {
		case err := <-p.logouts:
			t.Errorf("logged out before the end: %v", err)
		default:
		}
	}
	icancel()
	wait(t, "acceptor's logout", x.logouts)
	wait(t, "initiator's logout", c.logouts)
	for _, s := range []*session.Session{trader, broker} {
		if n := s.Stats().RejectsSent.Load(); n != 0 {
			t.Errorf("%s sent %d Rejects", s.ID(), n)
		}
	}
	if n := len(c.reports); n != 0 {
		t.Errorf("the initiator received %d ExecutionReports beyond those expected", n)
	}
	if got, want := broker.NextTargetSeqNum(), trader.NextSenderSeqNum(); got != want {
		t.Errorf("acceptor expects MsgSeqNum %d next, the initiator would send %d", got, want)
	}
	if got, want := trader.NextTargetSeqNum(), broker.NextSenderSeqNum(); got != want {
		t.Errorf("initiator expects MsgSeqNum %d next, the acceptor would send %d", got, want)
	}
}

// checkLife checks the ExecutionReports of order o tell its life: new, then
// filled trade by trade, each report adding up.
func checkLife(t *testing.T, o order, reports []report) {
	t.Helper()
	if len(reports) != fills+1 {
		t.Errorf("order %s: %d ExecutionReports, want %d", o.ClOrdID, len(reports), fills+1)
		return
	}
	var cum float64
	for i, r := range reports {
		wantExec, wantStatus := execTrade, statusPartial
		switch {
		case i == 0:
			wantExec, wantStatus = execNew, statusNew
		case i == fills:
			wantStatus = statusFilled
		}
		cum += r.LastQty
		switch {
		case r.ExecType != wantExec || r.OrdStatus != wantStatus:
			t.Errorf("order %s, report %d: ExecType %s, OrdStatus %s; want %s, %s", o.ClOrdID, i, r.ExecType, r.OrdStatus, wantExec, wantStatus)
		case r.Symbol != o.Symbol || r.Side != o.Side || r.OrderQty != o.Qty || r.Price != o.Price:
			t.Errorf("order %s, report %d: %s %s %v@%v; want %s %s %v@%v", o.ClOrdID, i, r.Side, r.Symbol, r.OrderQty, r.Price, o.Side, o.Symbol, o.Qty, o.Price)
		case r.CumQty != cum || r.CumQty+r.LeavesQty != o.Qty:
			t.Errorf("order %s, report %d: CumQty %v, LeavesQty %v after %v filled of %v", o.ClOrdID, i, r.CumQty, r.LeavesQty, cum, o.Qty)
		case i > 0 && (r.LastQty <= 0 || r.LastPx != o.Price || r.AvgPx != o.Price):
			t.Errorf("order %s, report %d: filled %v at %v, average %v; want a fill at %v", o.ClOrdID, i, r.LastQty, r.LastPx, r.AvgPx, o.Price)
		}
	}
}

func sameReports(a, b []report) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].equal(b[i]) {
			return false
		}
	}
	return true
}

func countReports(byOrder map[string][]report) (n int) {
	for _, reports := range byOrder {
		n += len(reports)
	}
	return n
}

// wait waits for an event on ch, failing the test after timeout.
func wait[T any](t *testing.T, what string, ch <-chan T) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for the %s", what)
	}
}
