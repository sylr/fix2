package session

import (
	"context"
	"time"
)

// Header is an SBE message header, as SBE's standard one starts: every
// message a FIXP session carries starts so (docs/fixp.md).
type Header struct {
	BlockLength, TemplateID, SchemaID, Version uint16
}

// Inbound describes a received application message.
type Inbound struct {
	Header Header
	// Seq is its sequence number on a sequenced flow, 0 on an Unsequenced
	// one.
	Seq uint64
	// Retransmission is set for a message of a Retransmission batch.
	Retransmission bool
	// Received is when its frame's last byte was read.
	Received time.Time
}

// Handler receives a session's events and application messages.
//
// Callbacks run one at a time per session, in order, on its reader
// goroutine: they hold back what follows, and must return promptly. The
// []byte they get are borrowed for the call. Their ctx tells the session
// they run from a callback: a Send with it never waits for room in the
// queue, failing with ErrQueueFull instead, and Terminate and Finish never
// wait anyway. Embed BaseHandler for defaults.
type Handler interface {
	// OnEstablished tells the session is established on a connection,
	// before any message of that connection is delivered.
	OnEstablished(ctx context.Context, s *Session)
	// OnUnbound tells the connection is no longer bound to the session,
	// after the last message of it delivered: err is a
	// *fixp.TerminatedError, or the read or write error that ended it. The
	// next connection's callbacks, and a retirement's OnFinalized, wait for
	// it to return.
	OnUnbound(ctx context.Context, s *Session, err error)

	// OnMessage delivers an application message, the SBE message whole,
	// its header included (docs/fixp.md, Delivery guarantees): on a
	// Recoverable flow in sequence, at least once, exactly once unless the
	// process stops between delivery and its record; on an Idempotent flow
	// at most once, in arrival order; on an Unsequenced one as it comes.
	// An error terminates the connection (UnspecifiedError), and the
	// message does not count as delivered: on a Recoverable flow it is
	// asked for again on the next establishment; on an Idempotent one it
	// was admitted, and is not delivered again. The error is logged; it
	// must not hold credentials. Applied and NotApplied are delivered
	// through their own callbacks.
	OnMessage(ctx context.Context, s *Session, msg []byte, in Inbound) error
	// OnApplied delivers the peer's Applied (§4.8.1): our messages from to
	// from+count-1 were applied. An error is OnMessage's.
	OnApplied(ctx context.Context, s *Session, from uint64, count uint32, in Inbound) error
	// OnNotApplied delivers the peer's NotApplied (§4.8.2): our messages
	// from to from+count-1 never reached it. Sending them again, under new
	// sequence numbers, is the application's choice; one that cannot be
	// made now (ErrQueueFull) returns an error, which is OnMessage's.
	OnNotApplied(ctx context.Context, s *Session, from uint64, count uint32, in Inbound) error

	// OnGap tells, on an Idempotent inbound flow, that the peer's messages
	// from to from+count-1 never came: the session reported them with
	// NotApplied if its outbound flow is sequenced, and goes on.
	OnGap(ctx context.Context, s *Session, from, count uint64)
	// OnLost tells, on a Recoverable inbound flow, that the peer no longer
	// holds its messages from to from+count-1 (RetransmitReject
	// OutOfRange). nil skips them, durably, and goes on; an error
	// terminates the connection, and the next establishment asks for them
	// again. (M6)
	OnLost(ctx context.Context, s *Session, from, count uint64) error

	// OnPeerFinished tells the peer finished its flow (FinishedSending),
	// whose every message is now delivered or skipped. The session answers
	// FinishedReceiving when it returns nil, and sends no application
	// message after: this is the last chance to send. An error terminates
	// the connection without answering: the flow stays unfinished, and the
	// peer's FinishedSending, repeated, calls it again. (M7)
	OnPeerFinished(ctx context.Context, s *Session) error
	// OnFinalized tells both flows are finished, or the session was
	// retired: it is over. (M7)
	OnFinalized(ctx context.Context, s *Session)
}

// BaseHandler implements every Handler method, doing nothing, and failing
// OnLost: nothing is skipped without the application.
type BaseHandler struct{}

func (BaseHandler) OnEstablished(context.Context, *Session)                    {}
func (BaseHandler) OnUnbound(context.Context, *Session, error)                 {}
func (BaseHandler) OnMessage(context.Context, *Session, []byte, Inbound) error { return nil }
func (BaseHandler) OnApplied(context.Context, *Session, uint64, uint32, Inbound) error {
	return nil
}
func (BaseHandler) OnNotApplied(context.Context, *Session, uint64, uint32, Inbound) error {
	return nil
}
func (BaseHandler) OnGap(context.Context, *Session, uint64, uint64)                {}
func (BaseHandler) OnLost(_ context.Context, _ *Session, from, count uint64) error { return errLost }
func (BaseHandler) OnPeerFinished(context.Context, *Session) error                 { return nil }
func (BaseHandler) OnFinalized(context.Context, *Session)                          {}
