package quickfixgo

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store/memstore"
)

// https://github.com/quickfixgo/quickfix/issues/162
//
// A store failing to persist a sent message went unnoticed: the message went
// out unstored, and could never be resent. The sender must get the error, and
// the message must neither be written nor take a sequence number.
func TestIssue162_StoreFailureReportedToSender(t *testing.T) {
	st := &i162Store{Store: memstore.New()}
	s, _, p := loggedOn(t, func(c *session.Config) { c.Store = st })
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	st.fail.Store(true)
	if _, err := s.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, "O1")); !errors.Is(err, errI162Disk) {
		t.Fatalf("Send = %v, want the store's error", err)
	}
	if next := s.NextSenderSeqNum(); next != 2 {
		t.Errorf("NextSenderSeqNum = %d after the failure, want 2", next)
	}
	p.expectNothing(50 * time.Millisecond)
	st.fail.Store(false)
	if _, err := s.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, "O2")); err != nil {
		t.Fatal(err)
	}
	p.expect("D", "34", "2", "11", "O2")
}

var errI162Disk = errors.New("disk full")

// i162Store fails Append, without storing, while fail is set.
type i162Store struct {
	*memstore.Store
	fail atomic.Bool
}

func (s *i162Store) Append(ctx context.Context, gen, seq uint64, raw []byte) error {
	if s.fail.Load() {
		return errI162Disk
	}
	return s.Store.Append(ctx, gen, seq, raw)
}
