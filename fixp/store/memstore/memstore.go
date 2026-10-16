// Package memstore is a FIXP store in memory: what it keeps is lost with
// the process, and a restart negotiates new sessions.
package memstore

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/store"
)

// ErrClosed is a call on a closed store or session.
var ErrClosed = errors.New("fixp/store/memstore: closed")

// Store is a store.Store in memory. The zero value is not usable: use New.
type Store struct {
	mu       sync.Mutex
	sessions map[fixp.SessionID]*record
	closed   bool
}

// record is a session's state and messages, shared by the Sessions opened
// on it.
type record struct {
	state store.State
	msgs  map[uint64][]byte
	low   uint64 // no message is kept below it
}

// New returns an empty Store.
func New() *Store {
	return &Store{sessions: map[fixp.SessionID]*record{}}
}

var _ store.Store = (*Store)(nil)

// Create implements store.Store.
func (s *Store) Create(ctx context.Context, meta store.Meta) (store.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if _, ok := s.sessions[meta.ID]; ok {
		return nil, store.ErrExists
	}
	r := &record{state: store.State{Meta: meta, NextOutbound: 1}, msgs: map[uint64][]byte{}, low: 1}
	s.sessions[meta.ID] = r
	return &session{s: s, r: r}, nil
}

// Open implements store.Store.
func (s *Store) Open(ctx context.Context, id fixp.SessionID) (store.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	r, ok := s.sessions[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &session{s: s, r: r}, nil
}

// Sessions implements store.Store.
func (s *Store) Sessions(ctx context.Context) ([]store.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	var states []store.State
	for _, r := range s.sessions {
		if r.state.FinalizedAt.IsZero() {
			states = append(states, r.state)
		}
	}
	return states, nil
}

// Forget implements store.Store.
func (s *Store) Forget(ctx context.Context, t time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	for id, r := range s.sessions {
		if f := r.state.FinalizedAt; !f.IsZero() && f.Before(t) {
			delete(s.sessions, id)
		}
	}
	return nil
}

// Close implements store.Store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// session is a store.Session on a record.
type session struct {
	s      *Store
	r      *record
	closed bool // under s.mu
}

var _ store.Session = (*session)(nil)

// lock locks the store, failing if it or the session is closed or ctx is
// done.
func (ss *session) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ss.s.mu.Lock()
	if ss.s.closed || ss.closed {
		ss.s.mu.Unlock()
		return ErrClosed
	}
	return nil
}

func (ss *session) State() store.State {
	ss.s.mu.Lock()
	defer ss.s.mu.Unlock()
	return ss.r.state
}

func (ss *session) SetEstablished(ctx context.Context, t time.Time) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	defer ss.s.mu.Unlock()
	if ss.r.state.EstablishedAt.IsZero() {
		ss.r.state.EstablishedAt = t
	}
	return nil
}

func (ss *session) SetNextOutbound(ctx context.Context, next uint64) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	defer ss.s.mu.Unlock()
	if next < ss.r.state.NextOutbound {
		return store.ErrSequence
	}
	ss.r.state.NextOutbound = next
	return nil
}

func (ss *session) Append(ctx context.Context, seq uint64, msg []byte) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	defer ss.s.mu.Unlock()
	if seq != ss.r.state.NextOutbound {
		return store.ErrSequence
	}
	ss.r.msgs[seq] = bytes.Clone(msg)
	ss.r.state.NextOutbound = seq + 1
	return nil
}

func (ss *session) Range(ctx context.Context, begin, end uint64, fn func(seq uint64, msg []byte) error) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	if begin == 0 || end >= ss.r.state.NextOutbound {
		ss.s.mu.Unlock()
		return store.ErrSequence
	}
	var msgs [][]byte
	for seq := begin; seq <= end; seq++ {
		m, ok := ss.r.msgs[seq]
		if !ok {
			ss.s.mu.Unlock()
			return store.ErrEvicted
		}
		msgs = append(msgs, m) // never modified: a snapshot
	}
	ss.s.mu.Unlock()
	for i, m := range msgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(begin+uint64(i), m); err != nil {
			return err
		}
	}
	return nil
}

func (ss *session) Evict(ctx context.Context, seq uint64) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	defer ss.s.mu.Unlock()
	for ; ss.r.low < seq; ss.r.low++ {
		delete(ss.r.msgs, ss.r.low)
	}
	return nil
}

func (ss *session) SetInbound(ctx context.Context, seq uint64) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	defer ss.s.mu.Unlock()
	if seq < ss.r.state.Inbound {
		return store.ErrSequence
	}
	ss.r.state.Inbound = seq
	return nil
}

func (ss *session) SetFinish(ctx context.Context, outbound, inbound store.Finish) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	defer ss.s.mu.Unlock()
	ss.r.state.OutboundFinish, ss.r.state.InboundFinish = outbound, inbound
	return nil
}

func (ss *session) Finalize(ctx context.Context, t time.Time) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	defer ss.s.mu.Unlock()
	if ss.r.state.FinalizedAt.IsZero() {
		ss.r.state.FinalizedAt = t
	}
	return nil
}

func (ss *session) Retire(ctx context.Context, t time.Time) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	defer ss.s.mu.Unlock()
	if ss.r.state.FinalizedAt.IsZero() {
		ss.r.state.FinalizedAt = t
		ss.r.state.Retired = true
	}
	return nil
}

func (ss *session) Sync(ctx context.Context) error {
	if err := ss.lock(ctx); err != nil {
		return err
	}
	ss.s.mu.Unlock()
	return nil
}

func (ss *session) Close() error {
	ss.s.mu.Lock()
	defer ss.s.mu.Unlock()
	ss.closed = true
	return nil
}
