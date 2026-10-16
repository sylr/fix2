package quickfixgo

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
)

// https://github.com/quickfixgo/quickfix/issues/447
//
// Persisting a sent message and incrementing the sender sequence number were
// two store calls: a failure between them left the message stored under a
// sequence number the store would hand out again. Storing a message must
// move the sequence number with it, and a write whose outcome is unknown
// must never be followed by a reuse of its sequence number.
func TestIssue447_StoredMessageMovesSeqNumAtomically(t *testing.T) {
	inner := memstore.New()
	st := &i447Store{Store: inner}
	s, _, p := loggedOn(t, func(c *session.Config) { c.Store = st })
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	st.fail.Store(true)
	if _, err := s.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, "O1")); !errors.Is(err, store.ErrUnknownOutcome) {
		t.Fatalf("Send = %v, want ErrUnknownOutcome", err)
	}
	// The session stays down rather than reuse sequence number 2.
	p.expectEnd()
	if state := s.State(); state != session.StateStopped {
		t.Errorf("State = %s, want stopped until the store is checked", state)
	}
	// What a restart finds: message 2 stored, and NextSender past it.
	state, err := inner.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.NextSender != 3 {
		t.Errorf("stored NextSender = %d, want 3: message 2 is stored", state.NextSender)
	}
	var stored []byte
	err = inner.Range(ctx, state.Generation, 2, 2, func(_ uint64, raw []byte, _ store.Hole) error {
		stored = append(stored, raw...)
		return nil
	})
	if m, perr := fix.Parse(stored); err != nil || perr != nil || string(m.Get(11)) != "O1" {
		t.Errorf("stored message 2 = %s (%v, %v), want order O1", visible(stored), err, perr)
	}
}

// i447Store, while fail is set, stores the message, then reports the outcome
// unknown, as a database whose commit acknowledgement is lost does.
type i447Store struct {
	*memstore.Store
	fail atomic.Bool
}

func (s *i447Store) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	if err := s.Store.Append(ctx, gen, seq, raw); err != nil || !s.fail.Load() {
		return err
	}
	return store.ErrUnknownOutcome
}
