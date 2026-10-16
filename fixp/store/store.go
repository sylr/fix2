// Package store defines what a FIXP session persists (docs/fixp.md,
// Persistence), and the stores that keep it: memstore in memory, storetest
// the conformance suite any store passes.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/sylr/fix2/fixp"
)

// Role is the side of a session a store keeps.
type Role uint8

const (
	Client Role = 1
	Server Role = 2
)

func (r Role) String() string {
	switch r {
	case Client:
		return "client"
	case Server:
		return "server"
	}
	return "unknown"
}

// Meta is what a session records once negotiated, and never changes. The
// negotiated flows are authoritative: a server whose configuration changes
// keeps them for the sessions it negotiated before.
type Meta struct {
	ID   fixp.SessionID
	Role Role
	// ClientFlow and ServerFlow are the flows negotiated: the client's
	// carries the client's messages to the server.
	ClientFlow, ServerFlow fixp.FlowType
	// Owner, on a server, is who negotiated the session, as its
	// authentication named it: an Establish authenticated as someone else
	// is rejected. Empty on a client.
	Owner string
	// NegotiatedAt is when the negotiation completed.
	NegotiatedAt time.Time
}

// Finish is where a flow is in finalization (§4.7).
type Finish struct {
	// Finishing is set once FinishedSending was sent (outbound) or received
	// (inbound), with its LastSeqNo, 0 on an unsequenced flow.
	Finishing bool
	LastSeqNo uint64
	// Finished is set once FinishedReceiving was received (outbound) or
	// sent (inbound).
	Finished bool
}

// State is a session's persisted state.
type State struct {
	Meta
	// EstablishedAt is when the session was first established, zero if
	// never: a server retires sessions negotiated and never established.
	EstablishedAt time.Time
	// NextOutbound is the next sequence number to assign on a sequenced
	// outbound flow, from 1.
	NextOutbound uint64
	// Inbound is, on a Recoverable inbound flow, the resolved frontier:
	// every sequence number up to it was delivered to the handler, which
	// returned, or skipped with its consent (Handler.OnLost); recovery
	// resumes after it. On an Idempotent one, the highest sequence number
	// admitted, past gaps reported: what is not above it is a duplicate.
	// 0 for none.
	Inbound uint64
	// Outbound and Inbound are the flows' finalization.
	OutboundFinish, InboundFinish Finish
	// FinalizedAt is when both flows finished, or the session was retired,
	// zero until then. Its ID stays refused until forgotten.
	FinalizedAt time.Time
	// Retired is set for a session finalized by retirement, its flows not
	// finished.
	Retired bool
}

var (
	// ErrExists is a Create for a session ID the store knows, finalized
	// ones it has not forgotten included.
	ErrExists = errors.New("fixp/store: session exists")
	// ErrNotFound is an Open for a session ID the store does not know.
	ErrNotFound = errors.New("fixp/store: no such session")
	// ErrEvicted is a Range over messages the store no longer keeps.
	ErrEvicted = errors.New("fixp/store: message evicted")
	// ErrSequence is an Append whose sequence number is not NextOutbound,
	// or a position moved backwards.
	ErrSequence = errors.New("fixp/store: out of sequence")
)

// Store keeps the sessions of a client or of a server. Its methods are
// safe for concurrent use. A method failing, its ctx done included, leaves
// the state as it was before it, or as it would be after it: a session
// whose store fails halts, and reads the truth from the store when opened
// again.
type Store interface {
	// Create records a newly negotiated session, durably before it
	// returns: a server sends NegotiationResponse after it. It fails with
	// ErrExists if the ID is known.
	Create(ctx context.Context, meta Meta) (Session, error)
	// Open returns a session the store knows, finalized or not.
	Open(ctx context.Context, id fixp.SessionID) (Session, error)
	// Sessions lists the states of the sessions the store knows that are
	// not finalized: a client resumes its own, a server those it may be
	// asked to establish.
	Sessions(ctx context.Context) ([]State, error)
	// Forget drops the sessions finalized before t, their IDs included:
	// the server's retention of the IDs it refuses to negotiate again.
	Forget(ctx context.Context, t time.Time) error
	// Close releases the store. Sessions it opened must not be used after.
	Close() error
}

// Session is the persisted state of one session, opened by one engine at a
// time, its methods called by one goroutine at a time. Each write is
// atomic: it happens whole or not at all, a crash included. Writes may be
// buffered: Sync makes every write before it durable, and the engine syncs
// before writing to the connection what depends on them (docs/fixp.md,
// Persistence).
type Session interface {
	// State returns the state as last written.
	State() State

	// SetEstablished records the first establishment, at t.
	SetEstablished(ctx context.Context, t time.Time) error
	// SetNextOutbound records the next sequence number to assign, on an
	// Idempotent flow, where messages are not kept. ErrSequence if it is
	// below the current one.
	SetNextOutbound(ctx context.Context, next uint64) error
	// Append stores msg, sent on a Recoverable flow with sequence number
	// seq, and sets NextOutbound to seq+1, atomically. seq must be
	// NextOutbound (ErrSequence). msg is borrowed for the call.
	Append(ctx context.Context, seq uint64, msg []byte) error
	// Range calls fn for every sequence number from begin to end
	// inclusive, in order, with its message, valid until fn returns. It
	// fails with ErrEvicted, before calling fn, if any of them is no longer
	// kept, and with ErrSequence if end is not below NextOutbound. It reads
	// a snapshot: an Evict meanwhile does not change what it reports. It
	// stops at fn's first error, and returns it.
	Range(ctx context.Context, begin, end uint64, fn func(seq uint64, msg []byte) error) error
	// Evict drops the messages below seq.
	Evict(ctx context.Context, seq uint64) error

	// SetInbound records the inbound position (State.Inbound). ErrSequence
	// if it is below the current one.
	SetInbound(ctx context.Context, seq uint64) error
	// SetFinish records the flows' finalization, atomically, and finalizes
	// the session at t once both are Finished.
	SetFinish(ctx context.Context, outbound, inbound Finish, t time.Time) error
	// Retire finalizes the session at t, its flows as they are.
	Retire(ctx context.Context, t time.Time) error

	// Sync makes every write before it durable.
	Sync(ctx context.Context) error
	// Close releases the session, not the store.
	Close() error
}
