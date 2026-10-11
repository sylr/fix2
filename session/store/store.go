// Package store defines how a FIX session persists its sequence numbers and
// the messages it sent, so that it can resend them when its counterparty
// asks and resume after a restart.
//
// A Store holds one session's state. Its sequence numbers belong to a
// generation, which starts at 1 and is bumped by Reset when both sides start
// over from sequence number 1 (a Logon with ResetSeqNumFlag, a new schedule
// window, a manual reset). Calls carry the generation they act on, so a
// late call from a previous generation cannot change the current one.
package store

import (
	"context"
	"errors"
	"time"
)

// State is a session's persisted state.
type State struct {
	Generation uint64    // current generation, from 1
	NextSender uint64    // MsgSeqNum of the next message sent
	NextTarget uint64    // MsgSeqNum expected of the next message received
	CreatedAt  time.Time // when the generation started
	WindowID   string    // the schedule window it started in, "" if none
}

// Hole classifies a sequence number Range has no message for.
type Hole uint8

const (
	// HoleNone: the position holds a message.
	HoleNone Hole = iota
	// HoleNotStored: the store chose not to keep this message (for
	// instance an administrative message); it is gap-filled.
	HoleNotStored
	// HoleRetention: the message was kept but has been truncated away.
	HoleRetention
	// HoleMissing: the message should be there but is not, a corruption the
	// session reports rather than gap-fills.
	HoleMissing
)

func (h Hole) String() string {
	switch h {
	case HoleNone:
		return "none"
	case HoleNotStored:
		return "not stored"
	case HoleRetention:
		return "beyond retention"
	case HoleMissing:
		return "missing"
	}
	return "unknown"
}

var (
	// ErrSeqMismatch reports an Append whose sequence number is not the
	// next one, or that would overwrite a different message: a bug, or a
	// second writer on the same store. The session stops.
	ErrSeqMismatch = errors.New("store: sequence number mismatch")
	// ErrGenerationGone reports a call for a generation that is no longer
	// the current one.
	ErrGenerationGone = errors.New("store: generation no longer current")
	// ErrUnknownOutcome reports that a write may or may not have been
	// committed. The session stops sending and never reuses the sequence
	// number.
	ErrUnknownOutcome = errors.New("store: write outcome unknown")
	// ErrBackwards reports a counter moved backwards within a generation.
	ErrBackwards = errors.New("store: sequence number moved backwards")
	// ErrClosed reports a call on a closed store.
	ErrClosed = errors.New("store: closed")
)

// Store persists one session's state.
//
// Append, SetNextTarget, SetNextSender and Reset are called by the session one
// at a time. Range and Sync may run concurrently with them, from another
// goroutine. A Store never calls back into the session, and never holds its
// own locks while calling a Range callback.
type Store interface {
	// Open loads the state, creating generation 1 with both sequence
	// numbers at 1 if the store is empty. The session calls it once, before
	// any other method; calling it again returns the current state.
	Open(ctx context.Context) (State, error)

	// Append stores raw, a message sent with sequence number seq in
	// generation gen, and sets NextSender to seq+1 atomically. seq must be
	// NextSender; repeating a successful Append with the same arguments
	// succeeds without effect, even though NextSender moved since (once the
	// message is truncated away, without comparing it). raw is borrowed for
	// the call. Append may buffer: durability is reached when Sync returns,
	// if the store implements Syncer.
	Append(ctx context.Context, gen, seq uint64, raw []byte) error

	// SetNextTarget records that every message up to next-1 was received
	// and processed. The session only lowers it on an explicit
	// administrative request (Session.SetNextTargetSeqNum).
	SetNextTarget(ctx context.Context, gen, next uint64) error

	// SetNextSender moves NextSender forward without storing messages, as a
	// SequenceReset the session sends does; the skipped numbers are holes
	// (HoleNotStored).
	SetNextSender(ctx context.Context, gen, next uint64) error

	// Range calls fn once for every sequence number from begin to end
	// inclusive, in order, in generation gen: with the message stored, or
	// with a nil raw and the reason there is none. It reports a snapshot
	// taken when it starts: end is lowered to NextSender-1, and Appends and
	// Truncates made meanwhile are not seen. raw is only valid until fn
	// returns. Range stops at fn's first error and returns it. It returns
	// ErrGenerationGone if gen is not the current generation, or stops being
	// it before Range returns.
	Range(ctx context.Context, gen, begin, end uint64, fn func(seq uint64, raw []byte, hole Hole) error) error

	// Reset starts generation expectGen+1: both sequence numbers back to 1,
	// the messages of earlier generations dropped (or archived), CreatedAt
	// and WindowID set. It is a no-op if that generation is already current,
	// and returns ErrGenerationGone if expectGen is older than the current
	// one.
	Reset(ctx context.Context, expectGen uint64, createdAt time.Time, windowID string) error

	// Close releases the store. The session owns its store and closes it.
	Close() error
}

// Syncer is a Store whose Appends become durable only once Sync returns,
// which lets it commit many messages at once.
type Syncer interface {
	// Sync makes every message of generation gen up to upToSeq durable.
	Sync(ctx context.Context, gen, upToSeq uint64) error
}

// Truncater is a Store that can drop old messages to bound its size; Range
// reports them as HoleRetention.
type Truncater interface {
	// Truncate drops the messages of generation gen below seq before.
	Truncate(ctx context.Context, gen, before uint64) error
}
