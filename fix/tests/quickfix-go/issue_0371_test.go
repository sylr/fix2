package quickfixgo

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/371
//
// Under load, resending stored messages panicked with an index out of range
// while parsing them, the race detector showing a message's bytes written by
// the reader while being serialized. Messages sent from several goroutines
// while the counterparty asks for them again must all go out, well formed,
// each one as it was stored.
func TestIssue371_ResendUnderLoad(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var (
		mu  sync.Mutex
		ids = map[int]string{} // ClOrdID by MsgSeqNum
	)
	errs := make(chan error, 4)
	// Rounds of 4 goroutines sending 20 messages each, the counterparty
	// asking for everything again as each round starts but the first.
	for round := range 4 {
		var wg sync.WaitGroup
		for g := range 4 {
			wg.Go(func() {
				for i := range 20 {
					id := fmt.Sprintf("R%dG%dO%d", round, g, i)
					r, err := s.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, id).SetString(55, "EURUSD"))
					if err != nil {
						errs <- err
						return
					}
					mu.Lock()
					ids[int(r.Seq())] = id
					mu.Unlock()
				}
			})
		}
		if round > 0 {
			p.send("2", 0, "7", "2", "16", "0")
		}
		wg.Wait()
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// Every message, fresh or resent, until all of them were seen.
	seen := map[int]bool{}
	for len(seen) < len(ids) {
		frame := p.next()
		m, err := fix.Parse(frame)
		if err != nil {
			t.Fatalf("malformed %s: %v", visible(frame), err)
		}
		switch string(m.MsgType()) {
		case "D":
		case "4", "0", "1":
			continue
		default:
			t.Fatalf("got %s", visible(frame))
		}
		seq, _ := fix.ParseInt(m.Get(34))
		if got, want := string(m.Get(11)), ids[seq]; got != want {
			t.Fatalf("message %d has ClOrdID %s, want %s: %s", seq, got, want, visible(frame))
		}
		seen[seq] = true
	}
}
