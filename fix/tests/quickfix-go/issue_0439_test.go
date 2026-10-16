package quickfixgo

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/session/store"
	"github.com/sylr/fix2/fix/session/store/memstore"
)

// https://github.com/quickfixgo/quickfix/issues/439
//
// Both sides ahead at logon, the initiator's earlier messages all
// administrative: the acceptor's Logon came too high, then its
// ResendRequest. The initiator sent its GapFill (NewSeqNo 6) before its own
// ResendRequest (5), which the acceptor refused as too low. Whatever the
// interleaving, no message may go out fresh below a NewSeqNo already sent,
// and the initiator's ResendRequest must go out.
func TestIssue439_BilateralResendInOrder(t *testing.T) {
	st := memstore.New()
	ctx := context.Background()
	gen, err := st.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Messages 1 to 3, our earlier Logons, are not kept: gap-filled.
	if err := st.SetNextSender(ctx, gen.Generation, 4); err != nil {
		t.Fatal(err)
	}
	s, _, l := startInitiator(t, func(c *session.Config) { c.Store = st })
	p := acceptPeer(t, l)
	logon := p.expect("A", "34", "4")
	// Our Logon too high, and our ResendRequest, at once.
	p.seq = 14
	b := p.build("A", 0, "98", "0", "108", string(logon.Get(108)))
	p.write(append(b, p.build("2", 0, "7", "1", "16", "0")...))

	var (
		resendRequest bool
		newSeqNo      int // the highest NewSeqNo of the GapFills
		fresh         = map[int]bool{4: true}
	)
	for !resendRequest || newSeqNo == 0 {
		m := p.next()
		msg, err := fix.Parse(m)
		if err != nil {
			t.Fatal(err)
		}
		seq, _ := fix.ParseInt(msg.Get(34))
		if string(msg.Get(43)) == "Y" {
			if string(msg.MsgType()) == "4" {
				n, _ := fix.ParseInt(msg.Get(36))
				newSeqNo = max(newSeqNo, n)
			}
			continue
		}
		if seq < newSeqNo {
			t.Fatalf("got %s after a GapFill to %d", visible(m), newSeqNo)
		}
		fresh[seq] = true
		if string(msg.MsgType()) == "2" {
			if string(msg.Get(7)) != "1" {
				t.Fatalf("got %s, want a ResendRequest from 1", visible(m))
			}
			resendRequest = true
		}
	}
	// Every message sent is either gap-filled or sent fresh.
	for seq := newSeqNo; seq < int(s.NextSenderSeqNum()); seq++ {
		if !fresh[seq] {
			t.Errorf("message %d neither gap-filled nor sent", seq)
		}
	}
}

// A ResendRequest of ours held behind a running replay, while another
// ResendRequest is queued behind it whose range covers ours: ours must go out
// before that replay gap-fills over it, not after.
func TestIssue439_HeldResendRequestBeforeQueuedReplay(t *testing.T) {
	st := &i439GatedStore{Store: memstore.New(), entered: make(chan struct{}), gate: make(chan struct{})}
	s, _, p := acceptedPeer(t, func(c *session.Config) { c.Store = st })
	for range 3 {
		if _, err := s.SendRaw(context.Background(), "B", fix.AppendTagValue(nil, 148, "x")); err != nil {
			t.Fatal(err)
		}
		p.expect("B")
	}
	// A first replay, stuck reading the store.
	st.arm(t)
	p.send("2", 0, "7", "2", "16", "0")
	st.waitEntered(t)
	// A message too high: our ResendRequest, held behind the replay. Then
	// a second ResendRequest, queued.
	p.send("B", 10, "148", "y")
	p.send("2", 0, "7", "2", "16", "0")
	waitFor(t, "the second ResendRequest", func() bool { return s.NextTargetSeqNum() == 4 })
	st.release()

	var (
		highest       int // the highest MsgSeqNum sent fresh or gap-filled
		resendRequest bool
	)
	for !resendRequest || highest < 5 {
		frame := p.next()
		m, err := fix.Parse(frame)
		if err != nil {
			t.Fatal(err)
		}
		seq, _ := fix.ParseInt(m.Get(34))
		if string(m.Get(43)) == "Y" {
			if string(m.MsgType()) == "4" {
				n, _ := fix.ParseInt(m.Get(36))
				highest = max(highest, n-1)
			}
			continue
		}
		if seq <= highest {
			t.Fatalf("got %s after %d was sent or gap-filled", visible(frame), highest)
		}
		highest = seq
		resendRequest = resendRequest || string(m.MsgType()) == "2"
	}
}

// What is sequenced after a ResendRequest queued behind a running replay
// stays held until that replay too is done: our own ResendRequest, or
// application messages.
func TestIssue439_HeldAfterQueuedReplayStayBehindIt(t *testing.T) {
	replays := []string{"B/2/Y", "B/3/Y", "B/4/Y", "B/2/Y", "B/3/Y", "B/4/Y"}
	t.Run("ResendRequest", func(t *testing.T) {
		s, p, st := i439Replaying(t)
		// Too high: queued for 2 to 4, then our ResendRequest, held.
		p.send("2", 10, "7", "2", "16", "0")
		waitFor(t, "our ResendRequest", func() bool { return s.NextSenderSeqNum() == 6 })
		st.release()
		if got, want := i439Order(t, p, 7), append(replays, "2/5/"); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	t.Run("application messages", func(t *testing.T) {
		s, p, st := i439Replaying(t)
		p.send("2", 0, "7", "2", "16", "0")
		waitFor(t, "the second ResendRequest", func() bool { return s.NextTargetSeqNum() == 4 })
		for range 2 {
			if _, err := s.SendRaw(context.Background(), "B", fix.AppendTagValue(nil, 148, "y")); err != nil {
				t.Fatal(err)
			}
		}
		st.release()
		if got, want := i439Order(t, p, 8), append(replays, "B/5/", "B/6/"); !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// i439Replaying returns an acceptor session that sent 3 messages, resending
// them at its counterparty's request from a store stuck until gate is closed.
func i439Replaying(t *testing.T) (*session.Session, *peer, *i439GatedStore) {
	t.Helper()
	st := &i439GatedStore{Store: memstore.New(), entered: make(chan struct{}), gate: make(chan struct{})}
	s, _, p := acceptedPeer(t, func(c *session.Config) { c.Store = st })
	for range 3 {
		if _, err := s.SendRaw(context.Background(), "B", fix.AppendTagValue(nil, 148, "x")); err != nil {
			t.Fatal(err)
		}
		p.expect("B")
	}
	st.arm(t)
	p.send("2", 0, "7", "2", "16", "0")
	st.waitEntered(t)
	return s, p, st
}

// i439Order reads n messages and returns their MsgType/MsgSeqNum/PossDupFlag.
func i439Order(t *testing.T, p *peer, n int) []string {
	t.Helper()
	var out []string
	for range n {
		m, err := fix.Parse(p.next())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(m.MsgType())+"/"+string(m.Get(34))+"/"+string(m.Get(43)))
	}
	return out
}

// i439GatedStore blocks its first Range once armed, until released.
type i439GatedStore struct {
	store.Store
	mu       sync.Mutex
	armed    bool
	entered  chan struct{}
	gate     chan struct{}
	released sync.Once
}

// arm blocks the next Range. The test releases it at the latest when it
// ends, before the session it runs is stopped: arm after starting it.
func (g *i439GatedStore) arm(t *testing.T) {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
	t.Cleanup(g.release)
}

func (g *i439GatedStore) release() { g.released.Do(func() { close(g.gate) }) }

// waitEntered waits for the blocked Range to start.
func (g *i439GatedStore) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for the replay")
	}
}

func (g *i439GatedStore) Range(ctx context.Context, gen, begin, end uint64, fn func(uint64, []byte, store.Hole) error) error {
	g.mu.Lock()
	armed := g.armed
	g.armed = false
	g.mu.Unlock()
	if armed {
		close(g.entered)
		<-g.gate
	}
	return g.Store.Range(ctx, gen, begin, end, fn)
}
