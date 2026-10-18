package session

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
)

// Inbound describes a received message to a Handler. A message received
// ahead of a gap keeps its Received when delivered, once the gap is filled:
// messages are delivered in sequence, not in the order of their Received.
type Inbound struct {
	Seq        uint64    // its MsgSeqNum
	Generation uint64    // the sequence number generation it belongs to
	PossDup    bool      // PossDupFlag (43): a retransmission
	PossResend bool      // PossResend (97): may have been sent before under another MsgSeqNum
	ApplVerID  string    // FIXT: the application version it was validated against
	Received   time.Time // when its last byte was read
}

// LogonRequest is a Logon a Handler authenticates before the session acts on
// it: before sequence numbers are reset or compared.
type LogonRequest struct {
	// ID is the session the Logon is for, as we name it.
	ID SessionID
	// Logon is the counterparty's Logon, valid during the call.
	Logon *fix.Message
	// Response is true when we sent our Logon first and this is the
	// counterparty's answer.
	Response bool
	// RemoteAddr is the counterparty's address.
	RemoteAddr net.Addr
	// TLS is the connection's TLS state, nil without TLS.
	TLS *tls.ConnectionState
	// Principal is who the transport authenticated, if it does.
	Principal any

	// byDefault is set by BaseHandler.Authenticate, which accepts any
	// Logon.
	byDefault bool
}

// Handler receives a session's events and application messages. Callbacks
// that receive a message get it borrowed for the duration of the call:
// Message.Clone it to keep it. Their ctx tells the session they run from a
// callback: a Receipt.Wait with it fails at once with ErrWaitInHandler, and
// a Send with it never waits. Embed BaseHandler for defaults.
type Handler interface {
	// Authenticate checks the counterparty's Logon before the session acts
	// on it. An error logs it out, with a generic Text: the error itself is
	// only logged, so as not to tell an unauthenticated peer why, and, for a
	// first Logon, is the Err of the LogoutError OnLogout gets; it must not
	// hold credentials. A Logon
	// with ResetSeqNumFlag the session refuses anyway (see Tolerations) is
	// not authenticated. It runs on the connection's reader goroutine.
	Authenticate(ctx context.Context, req *LogonRequest) error

	// OnLogon tells the session is logged on, before any application
	// message of the new logon is delivered.
	OnLogon(ctx context.Context, s *Session)
	// OnLogout tells the session is logged out or disconnected, after the
	// last application message delivered. reason says why, often a
	// *LogoutError. Except while recording the received messages processed
	// fails (see Config.Store): the messages received before the
	// disconnection, held until recording works again, the generation they
	// belong to is reset or the session stops, are delivered after
	// OnLogout. FromApp may then follow OnLogout, OnRecoveryLoss too: the
	// handler must stay ready for them once logged out.
	OnLogout(ctx context.Context, s *Session, reason error)

	// ToAdmin may add fields to a Logon (35=A) or Logout (35=5) before it
	// is sent, such as Username and Password. An error cancels a Logon: the
	// connection is closed.
	ToAdmin(ctx context.Context, s *Session, msgType string, b *fix.Builder) error
	// FromAdmin tells of an administrative message the session processed,
	// a Logout or Reject refusing our Logon included.
	// It runs on the connection's reader goroutine and must return quickly.
	FromAdmin(ctx context.Context, s *Session, m *fix.Message, in Inbound)
	// FromApp delivers an application message, in sequence order. An error
	// rejects it: a *RejectError with a Reject (35=3), a
	// *BusinessRejectError with a BusinessMessageReject (35=j),
	// fix.ErrUnsupportedMessageType (as generated Dispatch functions
	// return) with one whose reason is "unsupported message type", and any
	// other with one whose reason is "other". Either way the message counts
	// as processed. The error may be logged, its text sent: it must not hold
	// credentials.
	FromApp(ctx context.Context, s *Session, m *fix.Message, in Inbound) error

	// OnResend tells whether a stored application message may be resent at
	// the counterparty's request; false gap-fills it instead. It runs on
	// the connection's writer goroutine while new messages are held back,
	// so it must neither block nor take locks a Send caller may hold.
	OnResend(ctx context.Context, s *Session, seq uint64, m *fix.Message) bool
	// OnRecoveryLoss tells that messages begin to end may never be
	// delivered: the counterparty reset its sequence numbers past them, or
	// we could not resend messages of ours, ErrBeyondRetention telling those
	// evicted from the store, administrative ones among them. A
	// SequenceReset-Reset during a gap recovery reports each range never
	// received below the highest MsgSeqNum received, in a call of its own,
	// in sequence with the messages around it: those received ahead of the
	// gap are delivered, not lost. A GapFill received is not reported: the
	// session cannot tell administrative messages, which counterparties
	// gap-fill, from application ones they no longer keep.
	OnRecoveryLoss(ctx context.Context, s *Session, begin, end uint64, cause error)
}

// LogonHeader is the header of a Logon an initiator is about to send, as
// written on the wire, for a LogonSigner.
type LogonHeader struct {
	BeginString  string
	SenderCompID string
	TargetCompID string
	MsgSeqNum    uint64
	// SendingTime is the SendingTime (52) value.
	SendingTime string
}

// LogonSigner is implemented by Handlers whose counterparty authenticates
// the Logon with a signature of its header, as Binance does with Ed25519.
// SignLogon runs after ToAdmin, once the Logon's MsgSeqNum and SendingTime
// are fixed, outside the session's lock; the fields it sets on b, such as
// RawDataLength (95) and RawData (96) with Builder.SetData, are added to
// the Logon. An error cancels the Logon: the connection is closed. Only an
// initiator's Logon is signed.
type LogonSigner interface {
	SignLogon(ctx context.Context, s *Session, h LogonHeader, b *fix.Builder) error
}

// BaseHandler implements Handler with defaults: every Logon is accepted,
// every message resent, and events are ignored. An acceptor's session logs
// a warning the first time it logs on with a Logon BaseHandler's
// Authenticate accepted: a deployment exposed to counterparties it does not
// trust implements Authenticate, returning nil itself once a counterparty
// is authenticated, or authenticates in its transport.
type BaseHandler struct{}

var _ Handler = BaseHandler{}

func (BaseHandler) Authenticate(_ context.Context, req *LogonRequest) error {
	if req != nil {
		req.byDefault = true
	}
	return nil
}
func (BaseHandler) OnLogon(context.Context, *Session)         {}
func (BaseHandler) OnLogout(context.Context, *Session, error) {}
func (BaseHandler) ToAdmin(context.Context, *Session, string, *fix.Builder) error {
	return nil
}
func (BaseHandler) FromAdmin(context.Context, *Session, *fix.Message, Inbound) {}
func (BaseHandler) FromApp(context.Context, *Session, *fix.Message, Inbound) error {
	return nil
}
func (BaseHandler) OnResend(context.Context, *Session, uint64, *fix.Message) bool   { return true }
func (BaseHandler) OnRecoveryLoss(context.Context, *Session, uint64, uint64, error) {}

// Frame is a message as it went over the wire, for a MessageObserver.
type Frame struct {
	Raw        []byte // the whole message, valid during the call
	Seq        uint64
	Generation uint64
	Epoch      uint64 // the connection, numbered from 1
	Resend     bool   // a retransmission (PossDupFlag)
}

// MessageObserver sees every message a session sends and receives, such as
// for an audit log. Its methods run synchronously on the connection's reader
// and writer goroutines: they must neither block nor take locks a Send
// caller may hold. OnOutbound runs once the message is completely written.
type MessageObserver interface {
	OnInbound(s *Session, f Frame)
	OnOutbound(s *Session, f Frame)
}

// RejectObserver, implemented by a MessageObserver, is told of each Reject
// (35=3) the session sends of its own accord, not those a Handler's error
// asks for, and whether that rejection counted the message as received: one
// counted is not delivered, nor asked for again. One not counted was not
// consumed by it: received ahead of a gap, it is rejected again once handled
// in sequence; a duplicate was counted before; an administrative message may
// be counted later with no further word. OnRejected runs on the connection's
// reader goroutine once the frame that carried the message is handled, under
// the same constraints as OnInbound.
type RejectObserver interface {
	OnRejected(s *Session, r Rejected)
}

// Rejected is a message the session rejected.
type Rejected struct {
	// Seq is its MsgSeqNum, 0 if it had no valid one.
	Seq, Generation uint64
	Reason          dictionary.RejectReason
	// Counted reports whether this rejection counted it as received.
	Counted bool
}

// callbackKey marks a context passed to a Handler callback.
type callbackKey struct{}

// inCallback reports whether ctx is a Handler callback's.
func inCallback(ctx context.Context) bool {
	return ctx != nil && ctx.Value(callbackKey{}) != nil
}
