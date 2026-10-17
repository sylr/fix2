package binancefix

import (
	"context"
	"slices"
	"sync"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
)

// State is where a request stands. Queued and Written change; the others
// are final.
type State uint8

const (
	// Queued: waiting in the connection's queue, or being written.
	Queued State = iota + 1
	// Written: wholly written to the connection, no answer yet.
	Written
	// Answered: Binance answered every leg of it (see Receipt.Legs): with
	// an ExecutionReport, ExecutionReportAck, ListStatus,
	// OrderCancelReject, OrderAmendReject, OrderMassCancelReport,
	// LimitResponse or Reject. A leg may have been refused.
	Answered
	// StatusUnknown: Binance answered every leg, one at least with error
	// -1007 (TIMEOUT): "Send status unknown; execution status unknown."
	// That leg may or may not have been executed: reconcile, as for
	// PossiblyTransmitted.
	StatusUnknown
	// NeverWritten: the connection ended, or our Logout was queued, before
	// a byte of the request was written. Binance never saw it: it is safe
	// to send again. Nothing is resent by the client.
	NeverWritten
	// PossiblyTransmitted: the connection ended after the request was
	// written, or partly written, and before all its legs were answered
	// (Legs tells which were). Binance may have executed the others or
	// not: the application reconciles them (an order
	// status query, the account's ExecutionReports on the next
	// connection) before sending it again.
	PossiblyTransmitted
)

// Final reports whether s no longer changes.
func (s State) Final() bool { return s >= Answered }

func (s State) String() string {
	switch s {
	case Queued:
		return "queued"
	case Written:
		return "written"
	case Answered:
		return "answered"
	case StatusUnknown:
		return "status unknown"
	case NeverWritten:
		return "never written"
	case PossiblyTransmitted:
		return "possibly transmitted"
	}
	return "invalid"
}

// Answer is a message answering a request.
type Answer struct {
	// TemplateID is the answer's SBE template, such as
	// ExecutionReportTemplateID or RejectTemplateID.
	TemplateID uint16
	// SeqNum is the answer's sequence number on its connection.
	SeqNum uint32
	// ErrorCode is the answer's error code, if it has one: -1007 is
	// StatusUnknown.
	ErrorCode Optional[int32]
}

// errTimeout is Binance's TIMEOUT error code: "Timeout waiting for
// response from backend server. Send status unknown; execution status
// unknown."
const errTimeout = -1007

// Receipt follows a request from Send on. Its methods are safe for
// concurrent use.
type Receipt struct {
	epoch         uint64
	template      uint16
	ids           []string // the legs' IDs, which the connection indexes
	stopOnFailure bool     // a refused cancellation leaves the new order unattempted

	mu     sync.Mutex
	legs   []Leg
	state  State
	sent   bool   // a sequence number was assigned and writing began
	seq    uint32 // valid if sent
	order  uint64 // the request's rank among those written on the connection
	queued uint64 // the request's rank among those queued on the connection
	answer Answer
	err    error // why the connection ended, for NeverWritten and PossiblyTransmitted
	done   chan struct{}
}

func newReceipt(epoch uint64, template uint16, legs []Leg, stopOnFailure bool) *Receipt {
	r := &Receipt{epoch: epoch, template: template, legs: legs, stopOnFailure: stopOnFailure, state: Queued, done: make(chan struct{})}
	for _, l := range legs {
		r.ids = append(r.ids, l.ID)
	}
	return r
}

// Epoch returns the connection the request was sent on: requests are never
// carried over to another.
func (r *Receipt) Epoch() uint64 { return r.epoch }

// TemplateID returns the request's SBE template.
func (r *Receipt) TemplateID() uint16 { return r.template }

// IDs returns the identifiers answers are matched by, its legs': ClOrdID,
// CancelClOrdID, ClListID, the orders' ClOrdIDs, or ReqID. The slice is
// the caller's.
func (r *Receipt) IDs() []string { return slices.Clone(r.ids) }

// Legs returns the request's legs and how each was answered so far. The
// slice is the caller's.
func (r *Receipt) Legs() []Leg {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.legs)
}

// State returns where the request stands.
func (r *Receipt) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// SeqNum returns the request's sequence number, false until it is
// assigned, when its writing begins.
func (r *Receipt) SeqNum() (uint32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq, r.sent
}

// Answer returns the answer completing an Answered or StatusUnknown
// request: the last of its legs'.
func (r *Receipt) Answer() (Answer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.answer, r.state == Answered || r.state == StatusUnknown
}

// Err returns why a NeverWritten or PossiblyTransmitted request's
// connection ended, nil otherwise.
func (r *Receipt) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Done is closed once the request's state is final.
func (r *Receipt) Done() <-chan struct{} { return r.done }

// Wait waits for the request's final state, until ctx is done.
func (r *Receipt) Wait(ctx context.Context) (State, error) {
	select {
	case <-r.done:
		return r.State(), nil
	case <-ctx.Done():
		return r.State(), ctx.Err()
	}
}

// sending marks the request as being written with sequence number seq,
// order among the connection's. Its connection's lock is held.
func (r *Receipt) sending(seq uint32, order uint64) {
	r.mu.Lock()
	r.sent, r.seq, r.order = true, seq, order
	r.mu.Unlock()
}

// isSent reports whether the request's writing began.
func (r *Receipt) isSent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

// written marks a request wholly written, unless answered meanwhile.
func (r *Receipt) written() {
	r.mu.Lock()
	if r.state == Queued {
		r.state = Written
	}
	r.mu.Unlock()
}

// answered records what message m, answer a, tells of the request's legs.
// It reports whether m answered one, and whether the request is now
// answered, every leg.
func (r *Receipt) answered(m any, a Answer) (answers, final bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Final() || !r.apply(m, a) {
		return false, false
	}
	state := Answered
	for _, l := range r.legs {
		switch l.Outcome {
		case Pending:
			return true, false
		case Unknown:
			state = StatusUnknown
		}
	}
	r.state, r.answer = state, a
	close(r.done)
	return true, true
}

// end ends a request left unanswered by its connection's end, err:
// PossiblyTransmitted if its writing began, NeverWritten otherwise, unless
// unsent tells it did not reach the connection. It reports whether the
// request was still waiting.
func (r *Receipt) end(err error, unsent bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.Final() {
		return false
	}
	r.state, r.err = NeverWritten, err
	if r.sent && !unsent {
		r.state = PossiblyTransmitted
	}
	close(r.done)
	return true
}

// answerOf returns the Answer of message m of header h.
func answerOf(h fixsbe.MessageHeader, code Optional[int32]) Answer {
	return Answer{TemplateID: h.TemplateId, SeqNum: h.SeqNum, ErrorCode: code}
}
