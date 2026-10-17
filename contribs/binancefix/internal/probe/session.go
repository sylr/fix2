package probe

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
)

// session is a binancefix Client of one connection, its messages recorded.
type session struct {
	rn  *run
	c   *binancefix.Client
	h   *handler
	rec *recorder

	runDone chan struct{}
	runErr  error
	cancel  context.CancelFunc
	once    sync.Once
	logon   *LogonResult
}

// handler records which requests Binance's messages answered, and follows
// the orders placed.
type handler struct {
	binancefix.BaseHandler
	rec      *recorder
	orders   *tracker
	position *position
	logon    chan struct{}
	ended    chan struct{}
	once     sync.Once
	mu       sync.Mutex
	err      error
}

func (h *handler) OnLogon(binancefix.LogonEvent) { close(h.logon) }

func (h *handler) OnLogout(e binancefix.LogoutEvent) {
	h.mu.Lock()
	h.err = e.Err
	h.mu.Unlock()
	h.once.Do(func() { close(h.ended) })
}

// answered records the request a message answered, if any: by the ID of
// the leg it carries, among ids, or else the request's first.
func (h *handler) answered(in binancefix.Inbound, ids ...[]byte) {
	if in.Receipt == nil {
		return
	}
	rids := in.Receipt.IDs()
	id := rids[0]
	for _, c := range ids {
		if slices.Contains(rids, string(c)) {
			id = string(c)
			break
		}
	}
	h.rec.answer(in.Header.SeqNum, id)
}

func (h *handler) OnExecutionReport(in binancefix.Inbound, m *binancefix.ExecutionReport) {
	h.orders.execution(string(m.ClOrdID), string(m.OrigClOrdID), m.OrderID, m.ExecType, m.OrdStatus, m.ErrorCode)
	h.position.report(string(m.ClOrdID), string(m.OrigClOrdID), decimal{m.CumQty, m.QtyExponent}, m.OrdStatus, m.ErrorCode)
	h.answered(in, m.ClOrdID)
}

func (h *handler) OnExecutionReportAck(in binancefix.Inbound, m *binancefix.ExecutionReportAck) {
	h.orders.execution(string(m.ClOrdID), "", m.OrderID, m.ExecType, m.OrdStatus, m.ErrorCode)
	h.answered(in, m.ClOrdID)
}

func (h *handler) OnListStatus(in binancefix.Inbound, m *binancefix.ListStatus) {
	h.orders.list(m)
	h.answered(in, m.ClListID)
}

func (h *handler) OnReject(in binancefix.Inbound, _ *binancefix.Reject) { h.answered(in) }
func (h *handler) OnOrderCancelReject(in binancefix.Inbound, m *binancefix.OrderCancelReject) {
	h.answered(in, m.ClOrdID)
}
func (h *handler) OnOrderAmendReject(in binancefix.Inbound, m *binancefix.OrderAmendReject) {
	h.answered(in, m.ClOrdID)
}
func (h *handler) OnOrderMassCancelReport(in binancefix.Inbound, m *binancefix.OrderMassCancelReport) {
	h.answered(in, m.ClOrdID)
}
func (h *handler) OnLimitResponse(in binancefix.Inbound, m *binancefix.LimitResponse) {
	h.answered(in, m.ReqID)
}

// open connects and logs on as sender. It returns the session, also when
// it did not log on, and the Logon's result; the error tells why it did
// not.
func open(ctx context.Context, rn *run, sender string) (*session, *LogonResult, error) {
	o := rn.o
	rec := newRecorder()
	h := &handler{rec: rec, orders: rn.orders, position: rn.position, logon: make(chan struct{}), ended: make(chan struct{})}
	s := &session{rn: rn, h: h, rec: rec, runDone: make(chan struct{}), cancel: func() {}}
	lr := &LogonResult{Endpoint: o.Endpoint, SenderCompID: sender, SignatureTime: o.SignatureTime.String()}
	s.logon = lr
	c, err := binancefix.New(binancefix.Config{
		Dialer:        &tapDialer{d: o.dialer(), rec: rec},
		APIKey:        o.APIKey,
		PrivateKey:    o.PrivateKey,
		SenderCompID:  sender,
		SignatureTime: o.SignatureTime,
		HeartBtInt:    o.HeartBtInt,
		LogonTimeout:  o.Wait,
		Reconnect:     binancefix.Reconnect{Disabled: true, MaxRefusals: 1},
		Logger:        o.Logger,
		Handler:       h,
	})
	if err != nil {
		close(s.runDone)
		lr.Error = err.Error()
		return s, lr, err
	}
	s.c = c
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	go func() {
		defer close(s.runDone)
		s.runErr = c.Run(runCtx)
	}()
	select {
	case <-h.logon:
	case <-s.runDone:
	case <-ctx.Done():
		c.Stop()
		<-s.runDone
	}
	loggedOn := false
	select {
	case <-h.logon:
		loggedOn = true
	default:
	}
	s.fillLogon(loggedOn)
	if !loggedOn {
		s.close()
		err := s.runErr
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = errors.New("not logged on")
		}
		lr.Messages = rec.since(0)
		return s, lr, err
	}
	return s, lr, nil
}

// fillLogon completes the Logon's result from the messages recorded.
func (s *session) fillLogon(loggedOn bool) {
	lr := s.logon
	for _, m := range s.rec.since(0) {
		switch {
		case m.Sent && m.Type == "Logon" && lr.SignedSendingTime == "":
			lr.SignedSendingTime = s.rn.o.SignatureTime.Format(time.UnixMicro(m.sendingTime))
		case !m.Sent && m.ack != nil && lr.LogonAck == nil:
			lr.LogonAck = m.ack
		case !m.Sent && lr.LogonAck == nil && lr.Refusal == nil && (m.Type == "Reject" || m.Type == "Logout"):
			lr.Refusal = &Refusal{Message: m.Type, Text: m.Text, ErrorCode: m.ErrorCode, Fields: m.Fields}
		}
	}
	lr.Accepted = lr.LogonAck != nil
	if !loggedOn {
		if s.runErr != nil {
			lr.Error = s.runErr.Error()
		} else {
			lr.Error = "not logged on"
		}
	}
	lr.Messages = s.rec.since(0)
}

// alive reports whether the session is logged on.
func (s *session) alive() bool {
	if s.c == nil {
		return false
	}
	select {
	case <-s.h.ended:
		return false
	case <-s.runDone:
		return false
	default:
		return s.c.LoggedOn()
	}
}

// endErr returns why the connection ended.
func (s *session) endErr() error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if s.h.err != nil {
		return s.h.err
	}
	select {
	case <-s.runDone:
		return s.runErr
	default:
		return nil
	}
}

// close logs out, and waits for the client to be done.
func (s *session) close() {
	s.once.Do(func() {
		if s.c != nil {
			s.c.Stop()
			t := time.NewTimer(binancefix.DefaultLogoutTimeout + 5*time.Second)
			select {
			case <-s.runDone:
			case <-t.C:
				s.cancel()
				<-s.runDone
			}
			t.Stop()
		}
		s.cancel()
	})
}

// send sends m once the pacer lets it.
func (s *session) send(ctx context.Context, m binancefix.Request) (*binancefix.Receipt, error) {
	if err := s.rn.pace.wait(ctx); err != nil {
		return nil, err
	}
	return s.c.Send(m)
}

// await waits for r's answer, Options.Wait at most.
func (s *session) await(ctx context.Context, r *binancefix.Receipt) {
	t := time.NewTimer(s.rn.o.Wait)
	defer t.Stop()
	select {
	case <-r.Done():
	case <-t.C:
	case <-ctx.Done():
	}
}

// settle waits for late messages: Options.Settle.
func (s *session) settle(ctx context.Context) {
	t := time.NewTimer(s.rn.o.Settle)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// cancelReq returns the cancellation, of ClOrdID id, of order t: by its
// OrderID if known, which an amendment does not change, by its ClOrdID
// otherwise.
func (s *session) cancelReq(id string, t *tracked) *binancefix.OrderCancelRequest {
	m := &binancefix.OrderCancelRequest{ClOrdID: []byte(id), Symbol: []byte(s.rn.o.Symbol)}
	if oid, ok := s.rn.orders.orderID(t); ok {
		m.OrderID = binancefix.Some(oid)
	} else {
		m.OrigClOrdID = []byte(s.rn.orders.current(t))
	}
	return m
}

// cancelOpen cancels the orders of probe (all if "") still open, one at a
// time: cancelling an order of a list cancels the list.
func (s *session) cancelOpen(ctx context.Context, probe string) []Cleanup {
	var out []Cleanup
	for _, t := range s.rn.orders.open(probe) {
		if s.rn.orders.isDone(t) {
			continue
		}
		c := Cleanup{Probe: t.probe, ClOrdID: s.rn.orders.current(t)}
		if oid, ok := s.rn.orders.orderID(t); ok {
			c.OrderID = &oid
		}
		if ctx.Err() != nil || !s.alive() {
			c.Outcome, c.Detail = "not sent", fmt.Sprint(context.Cause(ctx))
			if ctx.Err() == nil {
				c.Detail = "connection lost"
			}
			out = append(out, c)
			continue
		}
		id := s.rn.id("x")
		r, err := s.send(ctx, s.cancelReq(id, t))
		if err != nil {
			c.Outcome, c.Detail = "not sent", err.Error()
			out = append(out, c)
			continue
		}
		s.await(ctx, r)
		leg, _ := legOf(r, binancefix.LegCancel, id)
		switch {
		case leg.Outcome == binancefix.Accepted:
			c.Outcome = "canceled"
			s.rn.orders.done(t)
		case leg.Outcome == binancefix.Refused && leg.Answer.ErrorCode.Valid && leg.Answer.ErrorCode.Value == -2011:
			// Unknown order: never placed, or done since.
			c.Outcome = "gone"
			s.rn.orders.done(t)
		case s.rn.orders.isDone(t):
			// Cancelled, as an ExecutionReport or a ListStatus told,
			// though not by the answer the client expects.
			c.Outcome = "canceled"
		case leg.Outcome != binancefix.Pending:
			c.Outcome, c.Detail = "refused", fmt.Sprintf("%s, error code %s", leg.Outcome, opt(leg.Answer.ErrorCode))
		default:
			c.Outcome, c.Detail = "unanswered", r.State().String()
		}
		out = append(out, c)
	}
	return out
}

// position is the base asset the probes' market orders bought (or sold)
// and did not trade back. Each order counts by its own reports: the
// largest CumQty they carried, final once one tells a final status (not
// -1007). Its fills are known then only: an order not final may have
// filled unseen, a TRADE delayed.
type position struct {
	mu     sync.Mutex
	orders map[string]*fill // by ClOrdID
	ids    []string         // in the order they were watched
}

type fill struct {
	side  binancefix.Side
	cum   big.Rat
	final bool
	// unknown: never resolved, its fills unknown (see session.resolve).
	unknown bool
}

func newPosition() *position { return &position{orders: map[string]*fill{}} }

// watch counts the fills of market order id, on side.
func (p *position) watch(id string, side binancefix.Side) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.orders[id] = &fill{side: side}
	p.ids = append(p.ids, id)
}

// any reports whether an order is watched.
func (p *position) any() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ids) > 0
}

func (d decimal) rat() *big.Rat {
	if d.e >= 0 {
		return new(big.Rat).SetInt(new(big.Int).Mul(big.NewInt(d.m), pow10(int(d.e))))
	}
	return new(big.Rat).SetFrac(big.NewInt(d.m), pow10(int(-d.e)))
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// report counts a report of an order, by its ClOrdID or OrigClOrdID (a
// cancellation's report): its CumQty cum, its status st and error code.
func (p *position) report(clOrdID, orig string, cum decimal, st binancefix.OrdStatus, code binancefix.Optional[int32]) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.orders[clOrdID]
	if f == nil {
		f = p.orders[orig]
	}
	if f == nil {
		return
	}
	if c := cum.rat(); c.Cmp(&f.cum) > 0 {
		f.cum.Set(c)
	}
	switch st {
	case binancefix.OrdStatusFilled, binancefix.OrdStatusCanceled, binancefix.OrdStatusRejected, binancefix.OrdStatusExpired:
		if !timeout(code) {
			f.final = true
		}
	}
}

// pending returns the orders whose fills are not known yet: not final,
// nor given up.
func (p *position) pending() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for _, id := range p.ids {
		if f := p.orders[id]; !f.final && !f.unknown {
			ids = append(ids, id)
		}
	}
	return ids
}

func (p *position) isFinal(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.orders[id]
	return f != nil && f.final
}

// giveUp records that the fills of order id cannot be known.
func (p *position) giveUp(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f := p.orders[id]; f != nil && !f.final {
		f.unknown = true
	}
}

// unknown returns the orders whose fills could not be known, with their
// side.
func (p *position) unknown() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var u []string
	for _, id := range p.ids {
		if f := p.orders[id]; f.unknown && !f.final {
			u = append(u, fmt.Sprintf("%s (%s)", id, f.side))
		}
	}
	return u
}

// net returns what was bought minus what was sold, as seen.
func (p *position) net() *big.Rat {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := new(big.Rat)
	for _, f := range p.orders {
		if f.side == binancefix.SideSell {
			n.Sub(n, &f.cum)
		} else {
			n.Add(n, &f.cum)
		}
	}
	return n
}

// flat returns the quantity and side of the trade that flattens the
// position as seen; ok is false when it is flat. The quantity has no more
// digits than needed: Binance refuses one more precise than its symbol's.
func (p *position) flat() (q decimal, side binancefix.Side, ok bool) {
	n := p.net()
	if n.Sign() == 0 {
		return decimal{}, 0, false
	}
	side = binancefix.SideSell
	if n.Sign() < 0 {
		side = binancefix.SideBuy
		n.Neg(n)
	}
	// Decimal: its denominator divides a power of ten.
	for e := 0; e <= 18; e++ {
		m := new(big.Rat).Mul(n, new(big.Rat).SetInt(pow10(e)))
		if m.IsInt() && m.Num().IsInt64() {
			return decimal{m.Num().Int64(), int8(-e)}, side, true
		}
	}
	return decimal{}, side, true
}

// settled reports whether the position is flat and every fill known.
func (p *position) settled() bool {
	_, _, unflat := p.flat()
	return !unflat && len(p.unknown()) == 0
}

// String describes the position left.
func (p *position) String() string {
	q, side, ok := p.flat()
	var s string
	switch {
	case !ok:
		s = "flat"
	case side == binancefix.SideSell:
		s = q.String() + " bought, not sold back"
	default:
		s = q.String() + " sold, not bought back"
	}
	if u := p.unknown(); len(u) > 0 {
		s = fmt.Sprintf("exposure unknown: the fills of %s unknown, as seen %s", strings.Join(u, ", "), s)
	}
	return s
}

// waitFinal waits, Wait at most, for a final report of market order id.
func (s *session) waitFinal(ctx context.Context, id string) bool {
	deadline := time.Now().Add(s.rn.o.Wait)
	for !s.rn.position.isFinal(id) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		t := time.NewTimer(10 * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
	return true
}

// resolve makes the fills of market order id known: it waits for its final
// report; none within Wait, it cancels the order, and waits Wait again for
// the report telling of its fills, the cancellation's CANCELED, or, the
// order no longer open (-2011: filled or expired unseen), its own late
// reports. It reports whether its fills are known.
func (s *session) resolve(ctx context.Context, id string) bool {
	if s.waitFinal(ctx, id) {
		return true
	}
	t := s.rn.orders.get(id)
	if t == nil || !s.alive() {
		return false
	}
	r, err := s.send(ctx, s.cancelReq(s.rn.id("x"), t))
	if err != nil {
		return false
	}
	s.await(ctx, r)
	return s.waitFinal(ctx, id)
}

// flatten trades the position back with a market order, once, and
// describes what it did. It first resolves the market orders whose fills
// are not known, and trades nothing while one is unknown: an unseen fill
// would be traded twice.
func (s *session) flatten(ctx context.Context, probe string) string {
	for _, id := range s.rn.position.pending() {
		if !s.resolve(ctx, id) {
			if ctx.Err() != nil {
				// Interrupted, not given up: the cleanup resolves it.
				return fmt.Sprintf("interrupted before the fills of %s were known: %s", id, s.rn.position)
			}
			s.rn.position.giveUp(id)
		}
	}
	if u := s.rn.position.unknown(); len(u) > 0 {
		return fmt.Sprintf("not traded back: %s", s.rn.position)
	}
	q, side, ok := s.rn.position.flat()
	if !ok {
		return "flat"
	}
	if q.m == 0 {
		return "too large to trade back: " + s.rn.position.String()
	}
	id := s.rn.id("fs")
	s.rn.position.watch(id, side)
	mark := s.rec.mark()
	r, err := s.place(ctx, probe, &binancefix.NewOrderSingle{PriceExponent: s.rn.price.e, QtyExponent: q.e, OrderQty: binancefix.Some(q.m),
		OrdType: binancefix.OrdTypeMarket, Side: side, ClOrdID: []byte(id), Symbol: []byte(s.rn.o.Symbol)})
	if err != nil {
		// Not sent: nothing of it to resolve.
		s.rn.position.report(id, "", decimal{}, binancefix.OrdStatusRejected, binancefix.Optional[int32]{})
		return fmt.Sprintf("%s %s not sent: %v", side, q, err)
	}
	s.await(ctx, r)
	if !s.resolve(ctx, id) && ctx.Err() == nil {
		s.rn.position.giveUp(id)
	}
	var last string
	for _, m := range received(s.rec.since(mark), func(m Message) bool { return isER(m) && (m.ClOrdID == id || m.OrigClOrdID == id) }) {
		last = short(m)
	}
	if last == "" {
		last = "no ExecutionReport, " + r.State().String()
	}
	return fmt.Sprintf("%s %s: %s; now %s", side, q, last, s.rn.position)
}

// legOf returns r's leg of kind k and ID id.
func legOf(r *binancefix.Receipt, k binancefix.LegKind, id string) (binancefix.Leg, bool) {
	if r == nil {
		return binancefix.Leg{}, false
	}
	for _, l := range r.Legs() {
		if l.Kind == k && l.ID == id {
			return l, true
		}
	}
	return binancefix.Leg{}, false
}

// pacer spaces requests by gap at least.
type pacer struct {
	mu   sync.Mutex
	gap  time.Duration
	last time.Time
	n    int
}

var errTooMany = fmt.Errorf("probe: more than %d requests: refusing to send more", maxRequests)

func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n >= maxRequests {
		return errTooMany
	}
	if d := time.Until(p.last.Add(p.gap)); d > 0 {
		if err := sleep(ctx, d); err != nil {
			return err
		}
	}
	p.last = time.Now()
	p.n++
	return nil
}

// tracked is an order a probe placed.
type tracked struct {
	probe string
	ids   []string // the ClOrdIDs it had, the last its current
	list  string
}

// clOrdID returns the order's current ClOrdID. An amendment, on the
// handler's goroutine, may change it: the tracker's lock is held, or ids
// read through it.
func (t *tracked) clOrdID() string { return t.ids[len(t.ids)-1] }

// current returns o's current ClOrdID.
func (t *tracker) current(o *tracked) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return o.clOrdID()
}

// tracker follows the orders placed by their ExecutionReports and
// ListStatus, to cancel those left open.
type tracker struct {
	mu        sync.Mutex
	all       []*tracked
	byClOrdID map[string]*tracked
	byOrderID map[int64]*tracked
	oid       map[*tracked]int64
	finished  map[*tracked]bool
	// uncertain are the orders answered with -1007 TIMEOUT: never taken
	// for done but by a status of their own, or a cancellation's answer.
	uncertain map[*tracked]bool
}

func newTracker() *tracker {
	return &tracker{byClOrdID: map[string]*tracked{}, byOrderID: map[int64]*tracked{}, oid: map[*tracked]int64{},
		finished: map[*tracked]bool{}, uncertain: map[*tracked]bool{}}
}

// add follows the order of ClOrdID id, placed by probe, of list list ("" if
// none).
func (t *tracker) add(probe, id, list string) *tracked {
	t.mu.Lock()
	defer t.mu.Unlock()
	o := &tracked{probe: probe, ids: []string{id}, list: list}
	t.all = append(t.all, o)
	t.byClOrdID[id] = o
	return o
}

func (t *tracker) find(clOrdID, orig string, oid binancefix.Optional[int64]) *tracked {
	if oid.Valid {
		if o := t.byOrderID[oid.Value]; o != nil {
			return o
		}
	}
	if o := t.byClOrdID[clOrdID]; o != nil && clOrdID != "" {
		return o
	}
	if orig != "" {
		return t.byClOrdID[orig]
	}
	return nil
}

// timeout reports whether code is -1007, TIMEOUT: "execution status
// unknown", whatever the message's status says.
func timeout(code binancefix.Optional[int32]) bool { return code.Valid && code.Value == -1007 }

func (t *tracker) execution(clOrdID, orig string, oid binancefix.Optional[int64], et binancefix.ExecType, st binancefix.OrdStatus, code binancefix.Optional[int32]) {
	t.mu.Lock()
	defer t.mu.Unlock()
	o := t.find(clOrdID, orig, oid)
	if o == nil {
		return
	}
	if oid.Valid {
		t.oid[o], t.byOrderID[oid.Value] = oid.Value, o
	}
	if et == binancefix.ExecTypeReplaced && clOrdID != "" && t.byClOrdID[clOrdID] == nil {
		// Amended under a ClOrdID of its own: the order's, from now on.
		o.ids = append(o.ids, clOrdID)
		t.byClOrdID[clOrdID] = o
	}
	if timeout(code) {
		// Its status unknown: it may be live. It stays to cancel, which
		// an "unknown order" answer settles.
		t.uncertain[o] = true
		return
	}
	switch st {
	case binancefix.OrdStatusFilled, binancefix.OrdStatusCanceled, binancefix.OrdStatusRejected, binancefix.OrdStatusExpired:
		t.finished[o] = true
		delete(t.uncertain, o)
	}
}

func (t *tracker) list(m *binancefix.ListStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	done := m.ListOrderStatus == binancefix.ListOrderStatusAllDone || m.ListOrderStatus == binancefix.ListOrderStatusReject
	listed := map[*tracked]bool{}
	for _, e := range m.Orders {
		o := t.find(string(e.ClOrdID), "", e.OrderID)
		if o == nil {
			continue
		}
		listed[o] = true
		if e.OrderID.Valid {
			t.oid[o], t.byOrderID[e.OrderID.Value] = e.OrderID.Value, o
		}
		if timeout(e.ErrorCode) {
			t.uncertain[o] = true
		}
	}
	if !done {
		return
	}
	// A list done or refused is so for its orders, but those of status
	// unknown.
	for _, o := range t.all {
		if (listed[o] || (len(m.ClListID) > 0 && o.list == string(m.ClListID))) && !t.uncertain[o] {
			t.finished[o] = true
		}
	}
}

// get returns the order of ClOrdID id.
func (t *tracker) get(id string) *tracked {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byClOrdID[id]
}

// ids returns the ClOrdIDs order o had.
func (t *tracker) ids(o *tracked) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(o.ids)
}

func (t *tracker) orderID(o *tracked) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.oid[o]
	return id, ok
}

func (t *tracker) isDone(o *tracked) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished[o]
}

func (t *tracker) done(o *tracked) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finished[o] = true
}

// open returns the orders of probe (all if "") not known to be done.
func (t *tracker) open(probe string) []*tracked {
	t.mu.Lock()
	defer t.mu.Unlock()
	var os []*tracked
	for _, o := range t.all {
		if !t.finished[o] && (probe == "" || o.probe == probe) {
			os = append(os, o)
		}
	}
	return os
}
