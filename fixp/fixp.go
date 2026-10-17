// Package fixp holds the values of the FIX Performance Session Layer
// (FIXP 1.0) that its engine (fixp/session) and stores (fixp/store) share:
// session IDs, flow types, reject and termination codes, and errors.
//
// docs/fixp.md says what fix2 implements of FIXP, and how.
package fixp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// SessionID identifies a logical session: an RFC 4122 version 4 UUID the
// client chooses, unique for all time (§3.4.1).
type SessionID [16]byte

// NewSessionID returns a random version 4 UUID.
func NewSessionID() SessionID {
	var id SessionID
	rand.Read(id[:]) // never fails (crypto/rand)
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id
}

// ParseSessionID parses a UUID in its canonical form,
// xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.
func ParseSessionID(s string) (SessionID, error) {
	var id SessionID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return id, fmt.Errorf("fixp: session ID %q: not a UUID", s)
	}
	hexs := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if _, err := hex.Decode(id[:], []byte(hexs)); err != nil {
		return id, fmt.Errorf("fixp: session ID %q: not a UUID", s)
	}
	return id, nil
}

// String returns id in its canonical form.
func (id SessionID) String() string {
	var b [36]byte
	hex.Encode(b[0:8], id[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], id[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], id[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], id[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], id[10:16])
	return string(b[:])
}

// Valid reports whether id is a version 4 UUID of the RFC 4122 variant, as
// a server requires (§3.4.1): the nil UUID is not.
func (id SessionID) Valid() bool { return id[6]>>4 == 4 && id[8]>>6 == 2 }

// FlowType is the delivery guarantee of a flow, one direction of a session
// (§3.4.4). Its values are the wire's.
type FlowType uint8

const (
	// Recoverable: exactly once; the receiver asks for what it missed.
	Recoverable FlowType = 0
	// Idempotent: at most once; the receiver reports gaps (NotApplied).
	Idempotent FlowType = 1
	// Unsequenced: best effort, no sequence numbers.
	Unsequenced FlowType = 2
	// None: no application message in that direction.
	None FlowType = 3
)

// Sequenced reports whether f numbers its messages: Recoverable and
// Idempotent flows do.
func (f FlowType) Sequenced() bool { return f == Recoverable || f == Idempotent }

func (f FlowType) String() string {
	switch f {
	case Recoverable:
		return "Recoverable"
	case Idempotent:
		return "Idempotent"
	case Unsequenced:
		return "Unsequenced"
	case None:
		return "None"
	}
	return fmt.Sprintf("FlowType(%d)", uint8(f))
}

// NegotiationRejectCode says why a server rejects a Negotiate (§4.2.3).
type NegotiationRejectCode uint8

const (
	NegotiationCredentials          NegotiationRejectCode = 0
	NegotiationFlowTypeNotSupported NegotiationRejectCode = 1
	NegotiationDuplicateID          NegotiationRejectCode = 2
	NegotiationUnspecified          NegotiationRejectCode = 3
)

// EstablishmentRejectCode says why a server rejects an Establish (§4.3.3).
type EstablishmentRejectCode uint8

const (
	EstablishmentUnnegotiated       EstablishmentRejectCode = 0
	EstablishmentAlreadyEstablished EstablishmentRejectCode = 1
	EstablishmentSessionBlocked     EstablishmentRejectCode = 2
	EstablishmentKeepaliveInterval  EstablishmentRejectCode = 3
	EstablishmentCredentials        EstablishmentRejectCode = 4
	EstablishmentUnspecified        EstablishmentRejectCode = 5
)

// RetransmitRejectCode says why a sender rejects a RetransmitRequest
// (§4.6.3).
type RetransmitRejectCode uint8

const (
	RetransmitOutOfRange           RetransmitRejectCode = 0
	RetransmitInvalidSession       RetransmitRejectCode = 1
	RetransmitRequestLimitExceeded RetransmitRejectCode = 2
)

// TerminationCode says why a side terminates (§4.4).
type TerminationCode uint8

const (
	TerminationFinished             TerminationCode = 0
	TerminationUnspecifiedError     TerminationCode = 1
	TerminationReRequestOutOfBounds TerminationCode = 2
	TerminationReRequestInProgress  TerminationCode = 3
)

// codeName returns names[c], the schema's name of the code, or the type's
// and the number for a code it does not know.
func codeName[C ~uint8](c C, names []string, typ string) string {
	if int(c) < len(names) {
		return names[c]
	}
	return fmt.Sprintf("%s(%d)", typ, uint8(c))
}

func (c NegotiationRejectCode) String() string {
	return codeName(c, []string{"Credentials", "FlowTypeNotSupported", "DuplicateId", "Unspecified"}, "NegotiationRejectCode")
}

func (c EstablishmentRejectCode) String() string {
	return codeName(c, []string{"Unnegotiated", "AlreadyEstablished", "SessionBlocked", "KeepaliveInterval", "Credentials", "Unspecified"}, "EstablishmentRejectCode")
}

func (c RetransmitRejectCode) String() string {
	return codeName(c, []string{"OutOfRange", "InvalidSession", "RequestLimitExceeded"}, "RetransmitRejectCode")
}

func (c TerminationCode) String() string {
	return codeName(c, []string{"Finished", "UnspecifiedError", "ReRequestOutOfBounds", "ReRequestInProgress"}, "TerminationCode")
}

// withReason appends ": reason" to s, if there is a reason.
func withReason(s, reason string) string {
	if reason == "" {
		return s
	}
	return s + ": " + reason
}

// NegotiationRejectedError is a server's NegotiationReject.
type NegotiationRejectedError struct {
	Code   NegotiationRejectCode
	Reason string
}

func (e *NegotiationRejectedError) Error() string {
	return withReason("fixp: negotiation rejected: "+e.Code.String(), e.Reason)
}

// EstablishmentRejectedError is a server's EstablishmentReject.
type EstablishmentRejectedError struct {
	Code   EstablishmentRejectCode
	Reason string
}

func (e *EstablishmentRejectedError) Error() string {
	return withReason("fixp: establishment rejected: "+e.Code.String(), e.Reason)
}

// TerminatedError tells why a connection was unbound: a Terminate sent or
// received, or an error that ended it without one.
type TerminatedError struct {
	Code   TerminationCode
	Reason string
	// ByPeer is true when the peer sent the first Terminate.
	ByPeer bool
	// Err is the cause on our side: a protocol violation (ErrProtocol), a
	// lapsed keepalive (ErrKeepaliveLapsed), a read or write error, nil
	// for a Terminate either side asked for.
	Err error
}

func (e *TerminatedError) Error() string {
	who := "us"
	if e.ByPeer {
		who = "the peer"
	}
	s := withReason(fmt.Sprintf("fixp: terminated by %s: %s", who, e.Code), e.Reason)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *TerminatedError) Unwrap() error { return e.Err }

var (
	// ErrProtocol is a protocol violation by the peer (§4.4): the connection
	// is terminated.
	ErrProtocol = errors.New("fixp: protocol violation")
	// ErrKeepaliveLapsed is the peer's silence beyond its keepalive
	// interval, leniency included.
	ErrKeepaliveLapsed = errors.New("fixp: keepalive interval lapsed")
	// ErrFinalized is a session whose flows are both finished: its ID can
	// no longer be established.
	ErrFinalized = errors.New("fixp: session finalized")
	// ErrNoFlow is a Send on a session whose outbound flow is None, or no
	// longer open (FinishedSending sent, or FinishedReceiving sent: §6.3).
	ErrNoFlow = errors.New("fixp: no outbound flow")
)
