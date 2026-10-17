package binancefix

import (
	"fmt"
	"slices"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
)

// LegKind is what part of a request a Leg is.
type LegKind uint8

const (
	// LegOrder is a new order: a NewOrderSingle, the new order of an
	// OrderCancelRequestAndNewOrderSingle, an order of a NewOrderList. An
	// ExecutionReport (or ExecutionReportAck) of ExecType NEW answers it,
	// or REJECTED; for a list's order, its entry in the list's ListStatus
	// too.
	LegOrder LegKind = iota + 1
	// LegCancel is a cancellation: an OrderCancelRequest, the cancellation
	// of an OrderCancelRequestAndNewOrderSingle (its CancelClOrdID). An
	// ExecutionReport of ExecType CANCELED answers it, or an
	// OrderCancelReject; never a ListStatus, which an earlier fill of the
	// list sends with the same IDs. Seen on the testnet (2026-10-17): the
	// CANCELED carries the cancellation's ClOrdID, the order's as
	// OrigClOrdID; a list's cancellation gets a ListStatus ALL_DONE
	// carrying the cancellation's ClOrdID as ClListID first, then the
	// CANCELED per order, which answer.
	LegCancel
	// LegAmend is an OrderAmendKeepPriorityRequest. An ExecutionReport of
	// ExecType REPLACED answers it, or an OrderAmendReject: its ClOrdID may
	// be the amended order's, whose fills (TRADE) do not answer it. Seen on
	// the testnet: REPLACED, OrdStatus NEW, carrying the amendment's
	// ClOrdID, the order's as OrigClOrdID.
	LegAmend
	// LegList is a NewOrderList: its ListStatus answers it.
	LegList
	// LegMassCancel is an OrderMassCancelRequest: its
	// OrderMassCancelReport answers it.
	LegMassCancel
	// LegLimits is a LimitQuery: its LimitResponse answers it.
	LegLimits
)

func (k LegKind) String() string {
	switch k {
	case LegOrder:
		return "order"
	case LegCancel:
		return "cancel"
	case LegAmend:
		return "amend"
	case LegList:
		return "list"
	case LegMassCancel:
		return "mass cancel"
	case LegLimits:
		return "limits"
	}
	return "invalid"
}

// Outcome is how a Leg was answered.
type Outcome uint8

const (
	// Pending: not answered yet; if the connection ends so, Binance may
	// have executed it or not.
	Pending Outcome = iota
	// Accepted: done, as asked (an order placed, cancelled, amended).
	Accepted
	// Refused: not done (rejected, or not attempted: the new order of an
	// OrderCancelRequestAndNewOrderSingle in STOP_ON_FAILURE mode whose
	// cancellation was refused).
	Refused
	// Unknown: answered with -1007 TIMEOUT, execution status unknown.
	Unknown
)

func (o Outcome) String() string {
	switch o {
	case Pending:
		return "pending"
	case Accepted:
		return "accepted"
	case Refused:
		return "refused"
	case Unknown:
		return "unknown"
	}
	return "invalid"
}

// Leg is a part of a request answered on its own: an order, a
// cancellation, an amendment, a list, a query. A request is answered once
// all its legs are.
type Leg struct {
	Kind LegKind
	// ID is what its answers carry: a ClOrdID, CancelClOrdID, ClListID or
	// ReqID.
	ID      string
	Outcome Outcome
	// Answer is the message that answered it, once not Pending.
	Answer Answer
}

// requestLegs returns the template of request m, its legs, and whether a
// refused cancellation leaves its new order unattempted.
func requestLegs(m Request) (uint16, []Leg, bool, error) {
	var tid uint16
	var legs []Leg
	stop := false
	leg := func(k LegKind, id []byte) { legs = append(legs, Leg{Kind: k, ID: string(id)}) }
	switch m := m.(type) {
	case *NewOrderSingle:
		tid = fixsbe.NewOrderSingleTemplateID
		leg(LegOrder, m.ClOrdID)
	case *OrderCancelRequest:
		tid = fixsbe.OrderCancelRequestTemplateID
		leg(LegCancel, m.ClOrdID)
	case *OrderCancelRequestAndNewOrderSingle:
		tid = fixsbe.OrderCancelRequestAndNewOrderSingleTemplateID
		// Binance names the cancellation with a ClOrdID of its own
		// without CancelClOrdID: its answer could not be told.
		if len(m.CancelClOrdID) == 0 {
			return 0, nil, false, fmt.Errorf("%w: CancelClOrdID required", ErrInvalidID)
		}
		leg(LegCancel, m.CancelClOrdID)
		leg(LegOrder, m.ClOrdID)
		stop = m.OrderCancelRequestAndNewOrderSingleMode == fixsbe.OrderCancelRequestAndNewOrderSingleModeStopOnFailure
	case *NewOrderList:
		tid = fixsbe.NewOrderListTemplateID
		leg(LegList, m.ClListID)
		for _, o := range m.Orders {
			leg(LegOrder, o.ClOrdID)
		}
	case *OrderAmendKeepPriorityRequest:
		tid = fixsbe.OrderAmendKeepPriorityRequestTemplateID
		leg(LegAmend, m.ClOrdID)
	case *OrderMassCancelRequest:
		tid = fixsbe.OrderMassCancelRequestTemplateID
		leg(LegMassCancel, m.ClOrdID)
	case *LimitQuery:
		tid = fixsbe.LimitQueryTemplateID
		leg(LegLimits, m.ReqID)
	default:
		return 0, nil, false, fmt.Errorf("%w: %T", ErrNotRequest, m)
	}
	for i, l := range legs {
		if !idPattern.MatchString(l.ID) {
			return 0, nil, false, fmt.Errorf("%w: %q", ErrInvalidID, l.ID)
		}
		if slices.ContainsFunc(legs[:i], func(o Leg) bool { return o.ID == l.ID }) {
			return 0, nil, false, fmt.Errorf("%w: %q twice in the request", ErrDuplicateID, l.ID)
		}
	}
	return tid, legs, stop, nil
}

// outcome returns Unknown for -1007, else Refused if refused, Accepted
// otherwise.
func outcome(code Optional[int32], refused bool) Outcome {
	switch {
	case code.Valid && code.Value == errTimeout:
		return Unknown
	case refused:
		return Refused
	}
	return Accepted
}

// answerIDs returns the IDs message m may answer a request by.
func answerIDs(m any) [][]byte {
	switch m := m.(type) {
	case *fixsbe.ExecutionReport:
		return [][]byte{m.ClOrdID}
	case *fixsbe.ExecutionReportAck:
		return [][]byte{m.ClOrdID}
	case *fixsbe.OrderCancelReject:
		return [][]byte{m.ClOrdID}
	case *fixsbe.OrderAmendReject:
		return [][]byte{m.ClOrdID}
	case *fixsbe.ListStatus:
		ids := [][]byte{m.ClListID}
		for _, o := range m.Orders {
			ids = append(ids, o.ClOrdID)
		}
		return ids
	case *fixsbe.OrderMassCancelReport:
		return [][]byte{m.ClOrdID}
	case *fixsbe.LimitResponse:
		return [][]byte{m.ReqID}
	}
	return nil
}

// apply records what message m, answer a, tells of r's legs, and reports
// whether it answered one. r's lock is held.
func (r *Receipt) apply(m any, a Answer) bool {
	setAnswer := func(i int, o Outcome, a Answer) bool {
		if r.legs[i].Outcome != Pending {
			return false
		}
		r.legs[i].Outcome, r.legs[i].Answer = o, a
		return true
	}
	set := func(i int, o Outcome) bool { return setAnswer(i, o, a) }
	find := func(k LegKind, id []byte) int {
		return slices.IndexFunc(r.legs, func(l Leg) bool { return l.Kind == k && l.ID == string(id) })
	}
	execution := func(id []byte, et fixsbe.ExecType, code Optional[int32]) bool {
		done := false
		if code.Valid && code.Value == errTimeout {
			for _, k := range []LegKind{LegOrder, LegCancel, LegAmend} {
				if i := find(k, id); i >= 0 {
					done = set(i, Unknown) || done
				}
			}
			return done
		}
		switch et {
		case fixsbe.ExecTypeNew:
			if i := find(LegOrder, id); i >= 0 {
				done = set(i, Accepted)
			}
		case fixsbe.ExecTypeCanceled:
			if i := find(LegCancel, id); i >= 0 {
				done = set(i, Accepted)
			}
		case fixsbe.ExecTypeReplaced:
			if i := find(LegAmend, id); i >= 0 {
				done = set(i, Accepted)
			}
		case fixsbe.ExecTypeRejected:
			for _, k := range []LegKind{LegOrder, LegCancel, LegAmend} {
				if i := find(k, id); i >= 0 {
					done = set(i, Refused) || done
				}
			}
		}
		// Other ExecTypes, TRADE, EXPIRED, are an order's life, not an
		// answer.
		return done
	}
	switch m := m.(type) {
	case *fixsbe.ExecutionReport:
		return execution(m.ClOrdID, m.ExecType, m.ErrorCode)
	case *fixsbe.ExecutionReportAck:
		return execution(m.ClOrdID, m.ExecType, m.ErrorCode)
	case *fixsbe.OrderCancelReject:
		// Its ClOrdID is the cancellation's, or the request's.
		i := find(LegCancel, m.ClOrdID)
		if i < 0 && slices.ContainsFunc(r.legs, func(l Leg) bool { return l.ID == string(m.ClOrdID) }) {
			i = slices.IndexFunc(r.legs, func(l Leg) bool { return l.Kind == LegCancel })
		}
		if i < 0 {
			return false
		}
		o := outcome(Some(m.ErrorCode), true)
		done := set(i, o)
		if done && r.stopOnFailure {
			// STOP_ON_FAILURE: the new order is not attempted (seen on
			// the testnet: its ExecutionReport REJECTED -2010, "order
			// placement not attempted", follows, the request answered).
			if j := slices.IndexFunc(r.legs, func(l Leg) bool { return l.Kind == LegOrder }); j >= 0 {
				set(j, o)
			}
		}
		return done
	case *fixsbe.OrderAmendReject:
		if i := find(LegAmend, m.ClOrdID); i >= 0 {
			return set(i, outcome(Some(m.ErrorCode), true))
		}
	case *fixsbe.ListStatus:
		// A ListStatus never answers a cancellation: one of an earlier
		// fill carries the same IDs. The cancellation's ExecutionReports
		// (CANCELED) or OrderCancelReject do: on the testnet, the
		// ListStatus ALL_DONE of a cancelled list, carrying the
		// cancellation's ClOrdID as ClListID, comes first, the CANCELED
		// after (TestListCancelAnsweredAfterListStatus).
		//
		// It is our list's when its ClListID is, or, ClListID being
		// optional, when one of its entries is one of our list's orders.
		// Each entry answers its order with its own error code.
		i := find(LegList, m.ClListID)
		refused := m.ListRejectReason.Valid || m.ListOrderStatus == fixsbe.ListOrderStatusReject
		done, ours, unknown := false, i >= 0, false
		for _, o := range m.Orders {
			j := find(LegOrder, o.ClOrdID)
			if j < 0 {
				continue
			}
			ours = true
			oc := outcome(o.ErrorCode, o.ErrorCode.Valid || o.OrdRejReason.Valid || refused)
			unknown = unknown || oc == Unknown
			oa := a
			oa.ErrorCode = o.ErrorCode
			done = setAnswer(j, oc, oa) || done
		}
		if !ours {
			return false
		}
		if i < 0 {
			i = slices.IndexFunc(r.legs, func(l Leg) bool { return l.Kind == LegList })
		}
		if i >= 0 {
			lo := outcome(Optional[int32]{}, refused)
			if unknown {
				lo = Unknown
			}
			done = set(i, lo) || done
		}
		if refused {
			// A refused list places none of its orders.
			for j, l := range r.legs {
				if l.Kind == LegOrder {
					done = set(j, Refused) || done
				}
			}
		}
		return done
	case *fixsbe.OrderMassCancelReport:
		if i := find(LegMassCancel, m.ClOrdID); i >= 0 {
			return set(i, outcome(m.ErrorCode, m.MassCancelResponse == fixsbe.MassCancelResponseCancelRequestRejected))
		}
	case *fixsbe.LimitResponse:
		if i := find(LegLimits, m.ReqID); i >= 0 {
			return set(i, Accepted)
		}
	case *fixsbe.Reject:
		// The whole request refused.
		done := false
		for i := range r.legs {
			done = set(i, outcome(m.ErrorCode, true)) || done
		}
		return done
	}
	return false
}
