// Package memstore is a store.Store keeping everything in memory: a session
// using it starts afresh, at sequence number 1, every time the process does.
package memstore

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sylr/fix2/fix/session/store"
)

// chunkSize is the size of the arena chunks messages are copied into; a
// larger message gets a chunk of its own.
const chunkSize = 64 << 10

// entry locates the message of a sequence number, or says why there is none.
type entry struct {
	chunk int // chunk number, from the store's first chunk ever
	off   uint32
	n     uint32
	hole  store.Hole
}

// Store is an in-memory store.Store. Messages are copied into chunks that are
// only ever appended to, at 8-byte aligned offsets, so that a message handed
// to a Range callback is never written again and is read without the lock.
// Truncate replaces the index rather than changing it, so a Range keeps the
// snapshot it took.
type Store struct {
	mu     sync.RWMutex
	state  store.State
	opened bool
	closed bool
	first  uint64   // sequence number of index[0]
	index  []entry  // sequence numbers first.. NextSender-1
	base   int      // chunk number of chunks[0]
	chunks [][]byte // arena, from chunk number base
	last   []uint64 // last sequence number in each chunk
}

var (
	_ store.Store     = (*Store)(nil)
	_ store.Syncer    = (*Store)(nil)
	_ store.Truncater = (*Store)(nil)
)

// New returns an empty Store.
func New() *Store { return &Store{} }

// Open implements store.Store.
func (s *Store) Open(context.Context) (store.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return store.State{}, store.ErrClosed
	}
	if !s.opened {
		s.opened = true
		s.state = store.State{Generation: 1, NextSender: 1, NextTarget: 1, CreatedAt: time.Now()}
		s.first = 1
	}
	return s.state, nil
}

// check returns an error if the store cannot act on generation gen.
func (s *Store) check(gen uint64) error {
	switch {
	case s.closed:
		return store.ErrClosed
	case !s.opened:
		return fmt.Errorf("memstore: not opened")
	case gen != s.state.Generation:
		return fmt.Errorf("%w: generation %d, current is %d", store.ErrGenerationGone, gen, s.state.Generation)
	}
	return nil
}

// Append implements store.Store.
func (s *Store) Append(_ context.Context, gen, seq uint64, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(gen); err != nil {
		return err
	}
	switch {
	case seq < s.state.NextSender:
		// A retry of an Append that succeeded is fine; anything else is not.
		// A message truncated away cannot be compared.
		if stored, hole := s.get(seq); hole == store.HoleRetention || hole == store.HoleNone && bytes.Equal(stored, raw) {
			return nil
		}
		return fmt.Errorf("%w: message %d is already stored", store.ErrSeqMismatch, seq)
	case seq > s.state.NextSender:
		return fmt.Errorf("%w: appending %d, next is %d", store.ErrSeqMismatch, seq, s.state.NextSender)
	}
	s.index = append(s.index, s.copyIn(seq, raw))
	s.state.NextSender = seq + 1
	return nil
}

// copyIn copies raw, the message seq, into the arena and returns its entry.
func (s *Store) copyIn(seq uint64, raw []byte) entry {
	n := len(raw)
	c := len(s.chunks) - 1
	if c < 0 || len(s.chunks[c])+n > cap(s.chunks[c]) {
		s.chunks = append(s.chunks, make([]byte, 0, max(chunkSize, n)))
		s.last = append(s.last, 0)
		c++
	}
	chunk := s.chunks[c]
	off := len(chunk)
	chunk = append(chunk, raw...)
	// Pad to 8 bytes: the next message never shares a word with this one.
	for len(chunk)%8 != 0 && len(chunk) < cap(chunk) {
		chunk = append(chunk, 0)
	}
	s.chunks[c] = chunk
	s.last[c] = seq
	return entry{chunk: s.base + c, off: uint32(off), n: uint32(n)}
}

// get returns the message stored for seq, or why there is none.
func (s *Store) get(seq uint64) ([]byte, store.Hole) {
	if seq < s.first {
		return nil, store.HoleRetention
	}
	return lookup(s.index[seq-s.first], s.chunks, s.base)
}

// lookup returns the message e locates in chunks, numbered from base.
func lookup(e entry, chunks [][]byte, base int) ([]byte, store.Hole) {
	if e.hole != store.HoleNone {
		return nil, e.hole
	}
	chunk := chunks[e.chunk-base]
	return chunk[e.off : e.off+e.n : e.off+e.n], store.HoleNone
}

// SetNextTarget implements store.Store.
func (s *Store) SetNextTarget(_ context.Context, gen, next uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(gen); err != nil {
		return err
	}
	s.state.NextTarget = next
	return nil
}

// SetNextSender implements store.Store.
func (s *Store) SetNextSender(_ context.Context, gen, next uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(gen); err != nil {
		return err
	}
	if next < s.state.NextSender {
		return fmt.Errorf("%w: NextSender %d after %d", store.ErrBackwards, next, s.state.NextSender)
	}
	for seq := s.state.NextSender; seq < next; seq++ {
		s.index = append(s.index, entry{hole: store.HoleNotStored})
	}
	s.state.NextSender = next
	return nil
}

// Range implements store.Store. It takes its snapshot under the lock, then
// reads it without: the entries and the bytes it refers to are never
// written again.
func (s *Store) Range(ctx context.Context, gen, begin, end uint64, fn func(seq uint64, raw []byte, hole store.Hole) error) error {
	s.mu.RLock()
	if err := s.check(gen); err != nil {
		s.mu.RUnlock()
		return err
	}
	begin, end = max(begin, 1), min(end, s.state.NextSender-1)
	first, index, base := s.first, s.index, s.base
	var chunks [][]byte
	if begin <= end {
		chunks = slices.Clone(s.chunks)
	}
	s.mu.RUnlock()
	for seq := begin; seq <= end; seq++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.current(gen); err != nil {
			return err
		}
		raw, hole := []byte(nil), store.HoleRetention
		if seq >= first {
			raw, hole = lookup(index[seq-first], chunks, base)
		}
		if err := fn(seq, raw, hole); err != nil {
			return err
		}
	}
	return s.current(gen)
}

// current returns an error if gen is not the current generation.
func (s *Store) current(gen uint64) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.check(gen)
}

// Reset implements store.Store.
func (s *Store) Reset(_ context.Context, expectGen uint64, createdAt time.Time, windowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return store.ErrClosed
	case !s.opened:
		return fmt.Errorf("memstore: not opened")
	case s.state.Generation == expectGen+1:
		return nil // already done
	case s.state.Generation != expectGen:
		return fmt.Errorf("%w: resetting generation %d, current is %d", store.ErrGenerationGone, expectGen, s.state.Generation)
	}
	s.state = store.State{Generation: expectGen + 1, NextSender: 1, NextTarget: 1, CreatedAt: createdAt, WindowID: windowID}
	s.first, s.index, s.base, s.chunks, s.last = 1, nil, 0, nil, nil
	return nil
}

// Sync implements store.Syncer: memory is as durable as it gets.
func (s *Store) Sync(_ context.Context, gen, _ uint64) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.check(gen)
}

// Truncate implements store.Truncater. The chunks holding only truncated
// messages are released.
func (s *Store) Truncate(_ context.Context, gen, before uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(gen); err != nil {
		return err
	}
	before = min(before, s.state.NextSender)
	if before <= s.first {
		return nil
	}
	// New slices: a Range in progress keeps reading the old ones.
	s.index = slices.Clone(s.index[before-s.first:])
	s.first = before
	k := 0
	for k < len(s.chunks)-1 && s.last[k] < before {
		k++
	}
	if k > 0 {
		s.chunks = slices.Clone(s.chunks[k:])
		s.last = slices.Clone(s.last[k:])
		s.base += k
	}
	return nil
}

// Close implements store.Store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.index, s.chunks, s.last = nil, nil, nil
	return nil
}
