package probe

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sylr/fix2/contribs/binancefix"
)

// catalog lists the probes, in the order they run.
var catalog = []Probe{
	{Name: "logon-seqnum", Letter: "a", run: probeLogonSeq,
		Assumption: "Binance's first message, the LogonAck, is numbered 1."},
	{Name: "order", Letter: "b", Priced: true, run: probeOrder,
		Assumption: "A limit order far from the market is answered by an ExecutionReport NEW, and its cancellation by an ExecutionReport CANCELED carrying the cancellation's ClOrdID."},
	{Name: "amend", Letter: "c", Priced: true, run: probeAmend,
		Assumption: "An OrderAmendKeepPriorityRequest is answered by an ExecutionReport REPLACED carrying its ClOrdID (or an OrderAmendReject)."},
	{Name: "cancel-replace", Letter: "d", Priced: true, run: probeCancelReplace,
		Assumption: "An OrderCancelRequestAndNewOrderSingle's cancellation is answered by an ExecutionReport CANCELED carrying its CancelClOrdID, or an OrderCancelReject carrying its CancelClOrdID or ClOrdID; its new order by an ExecutionReport NEW or REJECTED of its own, in either order; in STOP_ON_FAILURE mode, the cancellation refused (an OrderCancelReject), the new order is not attempted: Binance tells so by an ExecutionReport REJECTED -2010 \"order placement not attempted\", and the probe confirms no placement within a bounded observation window (--wait)."},
	{Name: "list-cancel", Letter: "e", Priced: true, run: probeListCancel,
		Assumption: "A NewOrderList is answered by its ListStatus; the list's cancellation by ExecutionReports CANCELED carrying the cancellation's ClOrdID (or an OrderCancelReject), never by a ListStatus alone."},
	{Name: "fill", Letter: "f", run: probeFill,
		Assumption: "An order filled at once still gets an ExecutionReport NEW first, then TRADE."},
	{Name: "mass-cancel", Letter: "g", Priced: true, run: probeMassCancel,
		Assumption: "An OrderMassCancelRequest is answered by an OrderMassCancelReport carrying its ClOrdID; the orders it cancels get ExecutionReports CANCELED, which do not answer it."},
	{Name: "limits", Letter: "h", run: probeLimits,
		Assumption: "A LimitQuery is answered by a LimitResponse carrying its ReqID."},
}

func check(name string, v Verdict, format string, args ...any) Check {
	return Check{Name: name, Verdict: v, Detail: fmt.Sprintf(format, args...)}
}

func notSent(name string, err error) Check {
	return check(name, Inconclusive, "not sent: %v", err)
}

// received returns the messages of ms received that f accepts.
func received(ms []Message, f func(Message) bool) []Message {
	var out []Message
	for _, m := range ms {
		if !m.Sent && f(m) {
			out = append(out, m)
		}
	}
	return out
}

func isER(m Message) bool { return m.Type == "ExecutionReport" || m.Type == "ExecutionReportAck" }

// definite reports whether m tells something for sure: not an error -1007
// (TIMEOUT, status unknown). Such a message is never evidence of another
// behaviour, only of uncertainty.
func definite(m Message) bool {
	if m.ErrorCode != nil && *m.ErrorCode == -1007 {
		return false
	}
	// A ListStatus: none of its entries of status unknown.
	for _, code := range m.OrderErrorCodes {
		if code == -1007 {
			return false
		}
	}
	return true
}

// short describes received message m: type, ExecType, sequence number, IDs
// and error.
func short(m Message) string {
	var b strings.Builder
	b.WriteString(m.Type)
	if m.ExecType != "" {
		b.WriteString(" " + strings.ToUpper(m.ExecType))
	}
	fmt.Fprintf(&b, " (seq %d", m.SeqNum)
	if m.ClOrdID != "" {
		fmt.Fprintf(&b, ", ClOrdID %s", m.ClOrdID)
	}
	if m.OrigClOrdID != "" {
		fmt.Fprintf(&b, ", OrigClOrdID %s", m.OrigClOrdID)
	}
	if m.ReqID != "" {
		fmt.Fprintf(&b, ", ReqID %s", m.ReqID)
	}
	if m.ErrorCode != nil {
		fmt.Fprintf(&b, ", error %d", *m.ErrorCode)
	}
	if m.Text != "" {
		fmt.Fprintf(&b, ", %q", m.Text)
	}
	b.WriteString(")")
	return b.String()
}

// answer describes the message that answered leg l, among ms.
func answer(ms []Message, l binancefix.Leg) string {
	for _, m := range ms {
		if !m.Sent && m.SeqNum == l.Answer.SeqNum && m.template == l.Answer.TemplateID {
			return short(m)
		}
	}
	return fmt.Sprintf("template %d (seq %d)", l.Answer.TemplateID, l.Answer.SeqNum)
}

// hint tells what to adjust for the refusal of an order or list, received
// as seq: a filter's refusal (-1013, "Filter failure: ...") calls for
// --price-far and --qty to fit the symbol's filters, which the probes
// cannot know. A refusal is never a contradiction: the case did not arise.
func hint(ms []Message, seq uint32) string {
	for _, m := range ms {
		if m.Sent || m.SeqNum != seq {
			continue
		}
		text := strings.ToLower(m.Text + " " + m.Fields)
		switch {
		case m.ErrorCode != nil && *m.ErrorCode == -1013, strings.Contains(text, "-1013"), strings.Contains(text, "filter"):
			return ": a filter of the symbol refused it; adjust --price-far and --qty to its filters (api/v3/exchangeInfo, api/v3/avgPrice on testnet.binance.vision) and run it again"
		case m.ErrorCode != nil && *m.ErrorCode == -2010, strings.Contains(text, "-2010"):
			return ": the account refused it (balance, order limits); check the testnet account"
		}
	}
	return " (price, quantity, balance?)"
}

// ended tells why r was not answered when its connection ended first, ""
// if it did not.
func ended(r *binancefix.Receipt) string {
	switch st := r.State(); st {
	case binancefix.NeverWritten, binancefix.PossiblyTransmitted:
		return fmt.Sprintf("the connection ended before an answer (%s: %v)", st, r.Err())
	}
	return ""
}

// unanswered judges the leg l of r that Binance did not answer as the
// client expects: Inconclusive, with why, if no other message explains it.
func (s *session) noAnswer(name string, r *binancefix.Receipt) Check {
	if why := ended(r); why != "" {
		return check(name, Inconclusive, "%s", why)
	}
	if ctx := s.rn.ctx; ctx != nil && ctx.Err() != nil {
		return check(name, Inconclusive, "interrupted before an answer: %v", context.Cause(ctx))
	}
	return check(name, Inconclusive, "no answer within %v", s.rn.o.Wait)
}

// mine returns a filter of the received messages about order t, or naming
// one of extra as ClOrdID.
func (s *session) mine(t *tracked, extra ...string) func(Message) bool {
	ids := s.rn.orders.ids(t)
	oid, hasOID := s.rn.orders.orderID(t)
	return func(m Message) bool {
		return slices.Contains(ids, m.ClOrdID) || slices.Contains(ids, m.OrigClOrdID) || slices.Contains(extra, m.ClOrdID) ||
			(hasOID && m.OrderID != nil && *m.OrderID == oid)
	}
}

func (s *session) limitBuy(id string) *binancefix.NewOrderSingle {
	p, q := s.rn.price, s.rn.qty
	return &binancefix.NewOrderSingle{PriceExponent: p.e, QtyExponent: q.e, OrderQty: binancefix.Some(q.m), OrdType: binancefix.OrdTypeLimit,
		Price: binancefix.Some(p.m), Side: binancefix.SideBuy, TimeInForce: binancefix.Some(binancefix.TimeInForceGoodTillCancel),
		ClOrdID: []byte(id), Symbol: []byte(s.rn.o.Symbol)}
}

// place sends new order m, followed as probe's.
func (s *session) place(ctx context.Context, probe string, m *binancefix.NewOrderSingle) (*binancefix.Receipt, error) {
	s.rn.orders.add(probe, string(m.ClOrdID), "")
	return s.send(ctx, m)
}

// placed places a limit order far from the market, and waits for its
// answer: the order, nil with why if it was not placed.
func (s *session) placed(ctx context.Context, probe, tag string) (*tracked, string) {
	mark := s.rec.mark()
	id := s.rn.id(tag)
	r, err := s.place(ctx, probe, s.limitBuy(id))
	if err != nil {
		return nil, "not sent: " + err.Error()
	}
	s.await(ctx, r)
	if l, _ := legOf(r, binancefix.LegOrder, id); l.Outcome != binancefix.Accepted {
		s.settle(ctx)
		return nil, s.orderCheck("", r, id, s.rec.since(mark)).Detail
	}
	return s.rn.orders.get(id), ""
}

// orderCheck judges the answer to the order leg id of r: an ExecutionReport
// NEW (or REJECTED) first.
func (s *session) orderCheck(name string, r *binancefix.Receipt, id string, ms []Message) Check {
	l, _ := legOf(r, binancefix.LegOrder, id)
	switch l.Outcome {
	case binancefix.Accepted:
		return check(name, Confirmed, "answered by %s", answer(ms, l))
	case binancefix.Refused:
		return check(name, Inconclusive, "refused by %s: no order placed%s", answer(ms, l), hint(ms, l.Answer.SeqNum))
	case binancefix.Unknown:
		return check(name, Inconclusive, "answered by %s: status unknown", answer(ms, l))
	}
	if why := ended(r); why != "" {
		return check(name, Inconclusive, "%s", why)
	}
	if ers := received(ms, func(m Message) bool { return definite(m) && isER(m) && m.ClOrdID == id }); len(ers) > 0 {
		return check(name, Contradicted, "its first ExecutionReport is %s, OrdStatus %s: neither NEW nor REJECTED, the client leaves the order unanswered",
			short(ers[0]), ers[0].OrdStatus)
	}
	return s.noAnswer(name, r)
}

// cancelCheck judges the answer to the cancellation leg id of r, of the
// orders ts, of list list if not "".
func (s *session) cancelCheck(name string, r *binancefix.Receipt, id string, ts []*tracked, list string, ms []Message) Check {
	l, _ := legOf(r, binancefix.LegCancel, id)
	others := func() string {
		var o []string
		for _, m := range received(ms, func(m Message) bool { return m.SeqNum != l.Answer.SeqNum }) {
			o = append(o, short(m))
		}
		if len(o) == 0 {
			return ""
		}
		return "; then " + strings.Join(o, ", ")
	}
	switch l.Outcome {
	case binancefix.Accepted:
		return check(name, Confirmed, "answered by %s%s", answer(ms, l), others())
	case binancefix.Refused:
		return check(name, Inconclusive, "refused by %s: nothing cancelled", answer(ms, l))
	case binancefix.Unknown:
		return check(name, Inconclusive, "answered by %s: status unknown", answer(ms, l))
	}
	if why := ended(r); why != "" {
		return check(name, Inconclusive, "%s", why)
	}
	about := func(m Message) bool {
		for _, t := range ts {
			if s.mine(t)(m) {
				return true
			}
		}
		return false
	}
	if ers := received(ms, func(m Message) bool {
		return definite(m) && isER(m) && m.execType == binancefix.ExecTypeCanceled && about(m)
	}); len(ers) > 0 {
		var d []string
		for _, m := range ers {
			d = append(d, short(m))
		}
		return check(name, Contradicted, "answered by %s, not carrying the cancellation's ClOrdID %s: the client leaves the cancellation unanswered",
			strings.Join(d, ", "), id)
	}
	listed := func(m Message) bool {
		return list != "" && m.Type == "ListStatus" && (m.ClListID == list || m.OrigClListID == list || slices.ContainsFunc(m.Orders, func(o string) bool {
			return slices.ContainsFunc(ts, func(t *tracked) bool { return slices.Contains(s.rn.orders.ids(t), o) })
		}))
	}
	if ls := received(ms, func(m Message) bool { return definite(m) && listed(m) }); len(ls) > 0 {
		return check(name, Contradicted, "answered by %s %s alone: the client never takes a ListStatus for a cancellation's answer, and leaves it unanswered",
			short(ls[0]), ls[0].Fields)
	}
	if rej := received(ms, func(m Message) bool { return definite(m) && m.Type == "OrderCancelReject" }); len(rej) > 0 {
		return check(name, Contradicted, "answered by %s, not carrying the cancellation's ClOrdID %s: the client leaves it unanswered", short(rej[0]), id)
	}
	// What came is of status unknown (-1007, in the message or in a
	// ListStatus's entries): no evidence of another behaviour.
	if u := received(ms, func(m Message) bool {
		return !definite(m) && (listed(m) || m.Type == "OrderCancelReject" || (isER(m) && about(m)))
	}); len(u) > 0 {
		return check(name, Inconclusive, "answered by %s %s: status unknown (-1007)", short(u[0]), u[0].Fields)
	}
	return s.noAnswer(name, r)
}

// probeLogonSeq: (a).
func probeLogonSeq(_ context.Context, s *session, _ string) []Check { return logonSeqChecks(s.logon) }

func logonSeqChecks(lr *LogonResult) []Check {
	const name = "LogonAck numbered 1"
	switch {
	case lr == nil || lr.LogonAck == nil:
		why := "no Logon"
		if lr != nil {
			why = lr.Error
		}
		return []Check{check(name, Inconclusive, "no LogonAck: %s", why)}
	case lr.LogonAck.SeqNum == 1:
		return []Check{check(name, Confirmed, "LogonAck seq 1")}
	}
	return []Check{check(name, Contradicted, "LogonAck seq %d: the client requires 1, and ends the connection (SequenceError)", lr.LogonAck.SeqNum)}
}

// probeOrder: (b).
func probeOrder(ctx context.Context, s *session, probe string) []Check {
	const (
		name1 = "limit order answered by an ExecutionReport NEW"
		name2 = "its cancellation answered by an ExecutionReport CANCELED carrying the cancellation's ClOrdID"
	)
	mark := s.rec.mark()
	id := s.rn.id("b")
	r, err := s.place(ctx, probe, s.limitBuy(id))
	if err != nil {
		return []Check{notSent(name1, err), check(name2, Inconclusive, "no order to cancel")}
	}
	s.await(ctx, r)
	s.settle(ctx)
	c1 := s.orderCheck(name1, r, id, s.rec.since(mark))
	if l, _ := legOf(r, binancefix.LegOrder, id); l.Outcome != binancefix.Accepted {
		return []Check{c1, check(name2, Inconclusive, "no order to cancel")}
	}
	t := s.rn.orders.get(id)
	mark = s.rec.mark()
	cid := s.rn.id("bx")
	rc, err := s.send(ctx, &binancefix.OrderCancelRequest{ClOrdID: []byte(cid), OrigClOrdID: []byte(s.rn.orders.current(t)), Symbol: []byte(s.rn.o.Symbol)})
	if err != nil {
		return []Check{c1, notSent(name2, err)}
	}
	s.await(ctx, rc)
	s.settle(ctx)
	return []Check{c1, s.cancelCheck(name2, rc, cid, []*tracked{t}, "", s.rec.since(mark))}
}

// probeAmend: (c).
func probeAmend(ctx context.Context, s *session, probe string) []Check {
	const name = "amendment answered by an ExecutionReport REPLACED carrying its ClOrdID"
	t, why := s.placed(ctx, probe, "c")
	if t == nil {
		return []Check{check(name, Inconclusive, "no order to amend: %s", why)}
	}
	mark := s.rec.mark()
	aid := s.rn.id("ca")
	half := s.rn.qty.half()
	r, err := s.send(ctx, &binancefix.OrderAmendKeepPriorityRequest{ClOrdID: []byte(aid), OrigClOrdID: []byte(s.rn.orders.current(t)),
		Symbol: []byte(s.rn.o.Symbol), QtyExponent: half.e, OrderQty: half.m})
	if err != nil {
		return []Check{notSent(name, err)}
	}
	s.await(ctx, r)
	s.settle(ctx)
	ms := s.rec.since(mark)
	l, _ := legOf(r, binancefix.LegAmend, aid)
	switch l.Outcome {
	case binancefix.Accepted:
		return []Check{check(name, Confirmed, "answered by %s, the quantity now %s", answer(ms, l), half)}
	case binancefix.Refused:
		return []Check{check(name, Inconclusive, "refused by %s: nothing amended", answer(ms, l))}
	case binancefix.Unknown:
		return []Check{check(name, Inconclusive, "answered by %s: status unknown", answer(ms, l))}
	}
	if why := ended(r); why != "" {
		return []Check{check(name, Inconclusive, "%s", why)}
	}
	if ers := received(ms, func(m Message) bool {
		return definite(m) && isER(m) && m.execType != binancefix.ExecTypeTrade && s.mine(t, aid)(m)
	}); len(ers) > 0 {
		return []Check{check(name, Contradicted, "answered by %s, not an ExecutionReport REPLACED carrying the amendment's ClOrdID %s: the client leaves the amendment unanswered",
			short(ers[0]), aid)}
	}
	if rej := received(ms, func(m Message) bool { return definite(m) && m.Type == "OrderAmendReject" }); len(rej) > 0 {
		return []Check{check(name, Contradicted, "answered by %s, not carrying the amendment's ClOrdID %s: the client leaves it unanswered", short(rej[0]), aid)}
	}
	return []Check{s.noAnswer(name, r)}
}

// xcn returns a cancel/replace of mode, of the cancellation cid of target
// (of ClOrdID ghost if nil, an order that never was), the new order nid
// a limit order far from the market.
func (s *session) xcn(mode binancefix.OrderCancelRequestAndNewOrderSingleMode, cid, nid string, target *tracked, ghost string) *binancefix.OrderCancelRequestAndNewOrderSingle {
	p, q := s.rn.price, s.rn.qty
	m := &binancefix.OrderCancelRequestAndNewOrderSingle{OrderCancelRequestAndNewOrderSingleMode: mode, PriceExponent: p.e, QtyExponent: q.e,
		OrderQty: binancefix.Some(q.m), OrdType: binancefix.OrdTypeLimit, Price: binancefix.Some(p.m), Side: binancefix.SideBuy,
		TimeInForce: binancefix.Some(binancefix.TimeInForceGoodTillCancel), CancelClOrdID: []byte(cid), ClOrdID: []byte(nid),
		Symbol: []byte(s.rn.o.Symbol), OrigClOrdID: []byte(ghost)}
	if target != nil {
		m.OrigClOrdID = []byte(s.rn.orders.current(target))
	}
	return m
}

// probeCancelReplace: (d).
func probeCancelReplace(ctx context.Context, s *session, probe string) []Check {
	stop, allow := binancefix.OrderCancelRequestAndNewOrderSingleModeStopOnFailure, binancefix.OrderCancelRequestAndNewOrderSingleModeAllowFailure
	const name1 = "STOP_ON_FAILURE, the cancellation accepted: an ExecutionReport CANCELED carrying the CancelClOrdID, the new order's ExecutionReport NEW"
	var checks []Check
	if t, why := s.placed(ctx, probe, "d"); t == nil {
		checks = append(checks, check(name1, Inconclusive, "no order to replace: %s", why))
	} else {
		checks = append(checks, s.cancelReplace(ctx, probe, name1, stop, t))
	}
	checks = append(checks,
		s.cancelReplace(ctx, probe, "STOP_ON_FAILURE, the cancellation refused: an OrderCancelReject, no placement of the new order observed during --wait", stop, nil),
		s.cancelReplace(ctx, probe, "ALLOW_FAILURE, the cancellation refused: an OrderCancelReject, the new order's own ExecutionReport", allow, nil))
	return checks
}

// cancelReplace sends a cancel/replace of mode cancelling target, or an
// order that never was if nil, and judges its answers.
func (s *session) cancelReplace(ctx context.Context, probe, name string, mode binancefix.OrderCancelRequestAndNewOrderSingleMode, target *tracked) Check {
	mark := s.rec.mark()
	cid, nid := s.rn.id("dx"), s.rn.id("dn")
	ghost := s.rn.id("dg")
	s.rn.orders.add(probe, nid, "")
	r, err := s.send(ctx, s.xcn(mode, cid, nid, target, ghost))
	if err != nil {
		return notSent(name, err)
	}
	s.await(ctx, r)
	s.settle(ctx)
	ms := s.rec.since(mark)
	lc, _ := legOf(r, binancefix.LegCancel, cid)
	lo, _ := legOf(r, binancefix.LegOrder, nid)
	if why := ended(r); why != "" {
		return check(name, Inconclusive, "%s", why)
	}
	// A Reject refuses the whole request before its cancellation is
	// processed: neither leg's answer is seen, the case did not arise.
	for _, l := range []binancefix.Leg{lc, lo} {
		if l.Outcome != binancefix.Pending && l.Answer.TemplateID == binancefix.RejectTemplateID {
			return check(name, Inconclusive, "the whole request refused by %s: neither the cancellation's answer nor the new order's arose", answer(ms, l))
		}
	}
	newERs := received(ms, func(m Message) bool { return isER(m) && m.ClOrdID == nid })

	// The cancellation.
	var cancelled string
	if target != nil {
		switch lc.Outcome {
		case binancefix.Accepted:
			cancelled = answer(ms, lc)
		case binancefix.Pending:
			if ers := received(ms, func(m Message) bool {
				return definite(m) && isER(m) && m.execType == binancefix.ExecTypeCanceled && s.mine(target)(m)
			}); len(ers) > 0 {
				return check(name, Contradicted, "the cancellation answered by %s, not carrying the CancelClOrdID %s: the client leaves it unanswered", short(ers[0]), cid)
			}
			return s.noAnswer(name, r)
		default:
			return check(name, Inconclusive, "the cancellation refused by %s: the case of an accepted one did not arise", answer(ms, lc))
		}
	} else {
		switch lc.Outcome {
		case binancefix.Refused:
			if lc.Answer.TemplateID != binancefix.OrderCancelRejectTemplateID {
				return check(name, Inconclusive, "the cancellation refused by %s, not an OrderCancelReject: the case did not arise", answer(ms, lc))
			}
			cancelled = answer(ms, lc)
			for _, m := range ms {
				if !m.Sent && m.SeqNum == lc.Answer.SeqNum {
					switch m.ClOrdID {
					case cid:
						cancelled += ", carrying the CancelClOrdID"
					case nid:
						cancelled += ", carrying the new order's ClOrdID"
					}
				}
			}
		case binancefix.Pending:
			if rej := received(ms, func(m Message) bool { return definite(m) && m.Type == "OrderCancelReject" }); len(rej) > 0 {
				return check(name, Contradicted, "the cancellation answered by %s, carrying neither the CancelClOrdID %s nor the ClOrdID %s: the client leaves it unanswered",
					short(rej[0]), cid, nid)
			}
			return s.noAnswer(name, r)
		default:
			return check(name, Inconclusive, "the cancellation of an order that never was answered by %s", answer(ms, lc))
		}
	}

	// The new order.
	// The new order.
	if target == nil && mode == binancefix.OrderCancelRequestAndNewOrderSingleModeStopOnFailure {
		// The OrderCancelReject seen, the new order is watched for Wait in
		// all, Binance's own timeout, on an uninterrupted connection. Every
		// report of it counts, whatever their order: one other than
		// REJECTED, or a ListStatus naming it, proves it was executed
		// (contradicted), which outweighs one of status unknown, -1007
		// (inconclusive), which outweighs the absence of either: no
		// placement observed, which proves nothing more.
		evidence := func(ms []Message) (executed, uncertain *Message) {
			for _, m := range received(ms, func(m Message) bool {
				return (isER(m) && (m.ClOrdID == nid || m.OrigClOrdID == nid)) ||
					(m.Type == "ListStatus" && (m.ClListID == nid || slices.Contains(m.Orders, nid)))
			}) {
				switch {
				case !definite(m):
					if uncertain == nil {
						uncertain = &m
					}
				case m.Type == "ListStatus" || m.execType != binancefix.ExecTypeRejected:
					if executed == nil {
						executed = &m
					}
				}
			}
			return executed, uncertain
		}
		if executed, _ := evidence(ms); executed == nil {
			// Uncertainty seen or not, execution may yet show.
			t := time.NewTimer(s.rn.o.Wait - s.rn.o.Settle)
			select {
			case <-t.C:
			case <-ctx.Done():
			case <-s.h.ended:
			}
			t.Stop()
			ms = s.rec.since(mark)
			newERs = received(ms, func(m Message) bool { return isER(m) && m.ClOrdID == nid })
		}
		executed, uncertain := evidence(ms)
		switch {
		case executed != nil:
			return check(name, Contradicted, "the cancellation answered by %s, yet the new order executed: %s; the client counts it refused, not attempted",
				cancelled, short(*executed))
		case uncertain != nil:
			return check(name, Inconclusive, "the cancellation answered by %s; the new order's status unknown: %s", cancelled, short(*uncertain))
		}
		switch {
		case ctx.Err() != nil:
			return check(name, Inconclusive, "the cancellation answered by %s; interrupted while watching for the new order", cancelled)
		case !s.alive():
			return check(name, Inconclusive, "the cancellation answered by %s; the connection lost while watching for the new order: %v", cancelled, s.endErr())
		}
		told := ""
		if len(newERs) > 0 {
			told = "; " + short(newERs[0]) + " told of it"
		}
		return check(name, Confirmed, "the cancellation answered by %s; no placement of the new order observed during %v (--wait), on an uninterrupted connection: consistent with not attempted, which an absence cannot prove%s",
			cancelled, s.rn.o.Wait, told)
	}
	switch lo.Outcome {
	case binancefix.Accepted, binancefix.Refused:
		if t := lo.Answer.TemplateID; t != binancefix.ExecutionReportTemplateID && t != binancefix.ExecutionReportAckTemplateID {
			return check(name, Inconclusive, "the new order answered by %s, not an ExecutionReport of its own: the case did not arise", answer(ms, lo))
		}
		return check(name, Confirmed, "the cancellation answered by %s; the new order by %s", cancelled, answer(ms, lo))
	case binancefix.Unknown:
		return check(name, Inconclusive, "the new order answered by %s: status unknown", answer(ms, lo))
	}
	for _, m := range newERs {
		if definite(m) {
			return check(name, Contradicted, "the new order's first ExecutionReport is %s: neither NEW nor REJECTED, the client leaves it unanswered", short(m))
		}
	}
	if len(newERs) > 0 {
		return check(name, Inconclusive, "the new order's status unknown: %s", short(newERs[0]))
	}
	// No answer is no evidence of another behaviour: a timeout.
	return check(name, Inconclusive, "the cancellation answered by %s, but nothing of the new order within %v", cancelled, s.rn.o.Wait)
}

// probeListCancel: (e).
func probeListCancel(ctx context.Context, s *session, probe string) []Check {
	const (
		name1 = "OTO list answered by its ListStatus"
		name2 = "its cancellation answered by ExecutionReports CANCELED carrying the cancellation's ClOrdID, not a ListStatus alone"
	)
	mark := s.rec.mark()
	l, w, p := s.rn.id("e"), s.rn.id("ew"), s.rn.id("ep")
	price, qty := s.rn.price, s.rn.qty
	leg := func(id string, side binancefix.Side) binancefix.NewOrderListOrders {
		return binancefix.NewOrderListOrders{PriceExponent: price.e, QtyExponent: qty.e, OrderQty: binancefix.Some(qty.m), OrdType: binancefix.OrdTypeLimit,
			Price: binancefix.Some(price.m), Side: side, TimeInForce: binancefix.Some(binancefix.TimeInForceGoodTillCancel),
			ClOrdID: []byte(id), Symbol: []byte(s.rn.o.Symbol)}
	}
	// The working order buys far from the market; the pending one, to sell
	// what it bought, is released once it is filled: never.
	pending := leg(p, binancefix.SideSell)
	pending.ListTriggeringInstructions = []binancefix.ListTriggeringInstructions{{ListTriggerType: binancefix.ListTriggerTypeFilled,
		ListTriggerTriggerIndex: 0, ListTriggerAction: binancefix.ListTriggerActionRelease}}
	m := &binancefix.NewOrderList{ContingencyType: binancefix.ContingencyTypeOneTriggersTheOther, ClListID: []byte(l),
		Orders: []binancefix.NewOrderListOrders{leg(w, binancefix.SideBuy), pending}}
	tw, tp := s.rn.orders.add(probe, w, l), s.rn.orders.add(probe, p, l)
	r, err := s.send(ctx, m)
	if err != nil {
		return []Check{notSent(name1, err), check(name2, Inconclusive, "no list to cancel")}
	}
	s.await(ctx, r)
	s.settle(ctx)
	ms := s.rec.since(mark)
	ll, _ := legOf(r, binancefix.LegList, l)
	var c1 Check
	switch ll.Outcome {
	case binancefix.Accepted:
		if !r.State().Final() {
			var pend []string
			for _, x := range r.Legs() {
				if x.Outcome == binancefix.Pending {
					pend = append(pend, x.ID)
				}
			}
			// Nothing answered them within Wait: no evidence either way.
			c1 = check(name1, Inconclusive, "answered by %s, its orders %s not within %v", answer(ms, ll), strings.Join(pend, ", "), s.rn.o.Wait)
		} else {
			c1 = check(name1, Confirmed, "answered by %s", answer(ms, ll))
		}
	case binancefix.Refused:
		c1 = check(name1, Inconclusive, "refused by %s: no list placed%s", answer(ms, ll), hint(ms, ll.Answer.SeqNum))
	case binancefix.Unknown:
		c1 = check(name1, Inconclusive, "answered by %s: status unknown", answer(ms, ll))
	default:
		if why := ended(r); why != "" {
			c1 = check(name1, Inconclusive, "%s", why)
		} else if ls := received(ms, func(m Message) bool { return m.Type == "ListStatus" }); len(ls) > 0 {
			c1 = check(name1, Contradicted, "%s %s, not matched to the list: the client leaves the list unanswered", short(ls[0]), ls[0].Fields)
		} else if ers := received(ms, func(m Message) bool { return isER(m) && (m.ClOrdID == w || m.ClOrdID == p) }); len(ers) > 0 {
			// A ListStatus missing within Wait is a timeout, not another
			// answer.
			c1 = check(name1, Inconclusive, "%s, but no ListStatus within %v", short(ers[0]), s.rn.o.Wait)
		} else {
			c1 = s.noAnswer(name1, r)
		}
	}
	if ll.Outcome != binancefix.Accepted {
		return []Check{c1, check(name2, Inconclusive, "no list to cancel")}
	}
	mark = s.rec.mark()
	cid := s.rn.id("ex")
	rc, err := s.send(ctx, &binancefix.OrderCancelRequest{ClOrdID: []byte(cid), OrigClListID: []byte(l), Symbol: []byte(s.rn.o.Symbol)})
	if err != nil {
		return []Check{c1, notSent(name2, err)}
	}
	s.await(ctx, rc)
	s.settle(ctx)
	return []Check{c1, s.cancelCheck(name2, rc, cid, []*tracked{tw, tp}, l, s.rec.since(mark))}
}

// probeFill: (f).
func probeFill(ctx context.Context, s *session, probe string) []Check {
	const name = "a market order filled at once gets an ExecutionReport NEW first, then TRADE"
	mark := s.rec.mark()
	id := s.rn.id("f")
	q := s.rn.qty
	// Its fills count in the position, whenever they come: what it
	// bought is traded back here, or by the cleanup.
	s.rn.position.watch(id, binancefix.SideBuy)
	r, err := s.place(ctx, probe, &binancefix.NewOrderSingle{PriceExponent: s.rn.price.e, QtyExponent: q.e, OrderQty: binancefix.Some(q.m),
		OrdType: binancefix.OrdTypeMarket, Side: binancefix.SideBuy, ClOrdID: []byte(id), Symbol: []byte(s.rn.o.Symbol)})
	if err != nil {
		// Not queued: nothing of it to resolve.
		s.rn.position.report(id, "", decimal{}, binancefix.OrdStatusRejected, binancefix.Optional[int32]{})
		return []Check{notSent(name, err)}
	}
	s.await(ctx, r)
	s.settle(ctx)
	ms := s.rec.since(mark)
	ers := received(ms, func(m Message) bool { return isER(m) && m.ClOrdID == id })
	c := s.fillCheck(name, r, id, ers, ms)
	if !s.rn.position.settled() || len(s.rn.position.pending()) > 0 {
		c.Detail += fmt.Sprintf("; %s; trading back: %s", s.rn.position, s.flatten(ctx, probe))
	}
	return []Check{c}
}

func (s *session) fillCheck(name string, r *binancefix.Receipt, id string, ers, ms []Message) Check {
	l, _ := legOf(r, binancefix.LegOrder, id)
	if len(ers) == 0 {
		switch {
		case l.Outcome == binancefix.Refused:
			return check(name, Inconclusive, "refused by %s", answer(ms, l))
		}
		return s.noAnswer(name, r)
	}
	// A -1007 report, wherever it is, leaves the order's status unknown:
	// the client takes it for StatusUnknown, which says nothing of what
	// comes first, the case does not arise.
	for _, m := range ers {
		if !definite(m) {
			return check(name, Inconclusive, "answered by %s: status unknown", short(m))
		}
	}
	first := ers[0]
	switch first.execType {
	case binancefix.ExecTypeNew:
		var trades []string
		for _, m := range ers[1:] {
			if m.execType == binancefix.ExecTypeTrade {
				trades = append(trades, strconv.Itoa(int(m.SeqNum)))
			}
		}
		if len(trades) > 0 {
			return check(name, Confirmed, "%s first, then TRADE (seq %s)", short(first), strings.Join(trades, ", "))
		}
		last := ers[len(ers)-1]
		return check(name, Inconclusive, "%s, then no TRADE (last %s): not filled at once, the case did not arise (liquidity of %s on the testnet?)",
			short(first), short(last), s.rn.o.Symbol)
	case binancefix.ExecTypeRejected:
		return check(name, Inconclusive, "refused by %s%s", short(first), hint(ms, first.SeqNum))
	}
	return check(name, Contradicted, "its first ExecutionReport is %s, OrdStatus %s, without NEW before: the client leaves the order unanswered until the connection ends (PossiblyTransmitted)",
		short(first), first.OrdStatus)
}

// probeMassCancel: (g).
func probeMassCancel(ctx context.Context, s *session, probe string) []Check {
	const (
		name1 = "mass cancellation answered by an OrderMassCancelReport carrying its ClOrdID"
		name2 = "the order it cancelled got an ExecutionReport CANCELED"
	)
	t, why := s.placed(ctx, probe, "g")
	if t == nil {
		return []Check{check(name1, Inconclusive, "no order to cancel: %s", why), check(name2, Inconclusive, "no order to cancel")}
	}
	mark := s.rec.mark()
	mc := s.rn.id("gm")
	r, err := s.send(ctx, &binancefix.OrderMassCancelRequest{ClOrdID: []byte(mc), Symbol: []byte(s.rn.o.Symbol),
		MassCancelRequestType: binancefix.MassCancelRequestTypeCancelSymbolOrders})
	if err != nil {
		return []Check{notSent(name1, err), check(name2, Inconclusive, "not sent")}
	}
	s.await(ctx, r)
	s.settle(ctx)
	ms := s.rec.since(mark)
	l, _ := legOf(r, binancefix.LegMassCancel, mc)
	var c1 Check
	switch l.Outcome {
	case binancefix.Accepted:
		c1 = check(name1, Confirmed, "answered by %s", answer(ms, l))
	case binancefix.Refused:
		c1 = check(name1, Inconclusive, "refused by %s", answer(ms, l))
	case binancefix.Unknown:
		c1 = check(name1, Inconclusive, "answered by %s: status unknown", answer(ms, l))
	default:
		if why := ended(r); why != "" {
			c1 = check(name1, Inconclusive, "%s", why)
		} else if reps := received(ms, func(m Message) bool { return definite(m) && m.Type == "OrderMassCancelReport" }); len(reps) > 0 {
			c1 = check(name1, Contradicted, "answered by %s, not carrying its ClOrdID %s: the client leaves it unanswered", short(reps[0]), mc)
		} else {
			c1 = s.noAnswer(name1, r)
		}
	}
	ers := received(ms, func(m Message) bool {
		return isER(m) && m.execType == binancefix.ExecTypeCanceled && s.mine(t)(m)
	})
	if len(ers) == 0 {
		return []Check{c1, check(name2, Inconclusive, "no ExecutionReport CANCELED of %s within %v", s.rn.orders.current(t), s.rn.o.Wait)}
	}
	whose := "another"
	switch ers[0].ClOrdID {
	case mc:
		whose = "the mass cancellation's"
	case s.rn.orders.current(t):
		whose = "the order's"
	}
	return []Check{c1, check(name2, Confirmed, "%s, carrying %s ClOrdID", short(ers[0]), whose)}
}

// probeLimits: (h).
func probeLimits(ctx context.Context, s *session, _ string) []Check {
	const name = "LimitQuery answered by a LimitResponse carrying its ReqID"
	mark := s.rec.mark()
	q := s.rn.id("h")
	r, err := s.send(ctx, &binancefix.LimitQuery{ReqID: []byte(q)})
	if err != nil {
		return []Check{notSent(name, err)}
	}
	s.await(ctx, r)
	s.settle(ctx)
	ms := s.rec.since(mark)
	l, _ := legOf(r, binancefix.LegLimits, q)
	switch l.Outcome {
	case binancefix.Accepted:
		fields := ""
		for _, m := range ms {
			if !m.Sent && m.SeqNum == l.Answer.SeqNum {
				fields = m.Fields
			}
		}
		return []Check{check(name, Confirmed, "answered by %s: %s", answer(ms, l), fields)}
	case binancefix.Refused, binancefix.Unknown:
		return []Check{check(name, Inconclusive, "refused by %s", answer(ms, l))}
	}
	if why := ended(r); why != "" {
		return []Check{check(name, Inconclusive, "%s", why)}
	}
	if resp := received(ms, func(m Message) bool { return definite(m) && m.Type == "LimitResponse" }); len(resp) > 0 {
		return []Check{check(name, Contradicted, "answered by %s, not carrying its ReqID %s: the client leaves it unanswered", short(resp[0]), q)}
	}
	return []Check{s.noAnswer(name, r)}
}
