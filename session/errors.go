package session

import (
	"errors"
	"fmt"

	"github.com/sylr/fix2/dictionary"
)

var (
	// ErrNotLoggedOn reports a Send while the session is not logged on.
	ErrNotLoggedOn = errors.New("session: not logged on")
	// ErrSlowConsumer reports a Send refused because the counterparty does
	// not read fast enough and the outbound queue is full.
	ErrSlowConsumer = errors.New("session: outbound queue full, counterparty not reading fast enough")
	// ErrReplayInProgress reports a Send refused while the session resends
	// messages its counterparty asked for and its held queue is full.
	ErrReplayInProgress = errors.New("session: resending messages, held queue full")
	// ErrResetting reports a Send refused while sequence numbers are being
	// reset.
	ErrResetting = errors.New("session: sequence numbers being reset")
	// ErrInCallback reports a Send from a handler callback that would have
	// had to wait.
	ErrInCallback = errors.New("session: cannot wait from a handler callback")
	// ErrWaitInHandler reports a Receipt.Wait from a handler callback: the
	// acknowledgement it waits for may need the callback to return.
	ErrWaitInHandler = errors.New("session: cannot wait for a receipt from a handler callback")
	// ErrStopped reports a call on a stopped session.
	ErrStopped = errors.New("session: stopped")
	// ErrSkipped reports a sent message that was gap-filled before the
	// counterparty acknowledged it.
	ErrSkipped = errors.New("session: message gap-filled before the counterparty acknowledged it")
	// ErrUnknownDelivery reports a sent message whose delivery cannot be
	// known any more: the sequence numbers were reset, or the session ended,
	// before the counterparty acknowledged it.
	ErrUnknownDelivery = errors.New("session: delivery unknown, sequence numbers reset or session ended first")
	// ErrReceiptUntracked reports a receipt too old to be tracked.
	ErrReceiptUntracked = errors.New("session: receipt no longer tracked")
)

// RejectError, returned by Handler.FromApp, makes the session reject the
// message with a session-level Reject (35=3) carrying Reason, the offending
// Tag (0 for none) and Text.
type RejectError struct {
	Reason dictionary.RejectReason
	Tag    int
	Text   string
}

func (e *RejectError) Error() string {
	if e.Tag != 0 {
		return fmt.Sprintf("session: reject: %s, tag %d: %s", e.Reason, e.Tag, e.Text)
	}
	return fmt.Sprintf("session: reject: %s: %s", e.Reason, e.Text)
}

// BusinessRejectError, returned by Handler.FromApp, makes the session reject
// the message with a BusinessMessageReject (35=j) carrying Reason
// (BusinessRejectReason, 380), RefID (BusinessRejectRefID, 379) and Text.
// Before FIX 4.2, which has no BusinessMessageReject, a Reject is sent.
type BusinessRejectError struct {
	Reason int
	RefID  string
	Text   string
}

func (e *BusinessRejectError) Error() string {
	return fmt.Sprintf("session: business reject %d: %s", e.Reason, e.Text)
}

// Business reject reasons (380).
const (
	BusinessRejectOther                        = 0
	BusinessRejectUnknownID                    = 1
	BusinessRejectUnknownSecurity              = 2
	BusinessRejectUnsupportedMessageType       = 3
	BusinessRejectApplicationNotAvailable      = 4
	BusinessRejectConditionallyRequiredMissing = 5
	BusinessRejectNotAuthorized                = 6
)

// LogoutError reports why a session was logged out or disconnected: the
// reason we gave, or the Text of the counterparty's Logout.
type LogoutError struct {
	ByCounterparty bool   // the counterparty logged out
	Text           string // the counterparty's Logout's Text, or why we logged out or disconnected
	Err            error  // the underlying error, if any
}

func (e *LogoutError) Error() string {
	who := "we logged out"
	if e.ByCounterparty {
		who = "counterparty logged out"
	}
	switch {
	case e.Text != "" && e.Err != nil:
		return fmt.Sprintf("session: %s: %s: %v", who, e.Text, e.Err)
	case e.Text != "":
		return fmt.Sprintf("session: %s: %s", who, e.Text)
	case e.Err != nil:
		return fmt.Sprintf("session: %s: %v", who, e.Err)
	}
	return "session: " + who
}

func (e *LogoutError) Unwrap() error { return e.Err }
