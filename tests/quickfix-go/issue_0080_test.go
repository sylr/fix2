package quickfixgo

import (
	"context"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/80
//
// A message sent was queued, and only written once something came in or a
// timer fired: on an idle session it waited. It must be written at once.
func TestIssue80_SendOnIdleSessionWrittenPromptly(t *testing.T) {
	s, _, p := loggedOn(t, nil) // HeartBtInt 30s: nothing else wakes the session
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, id := range []string{"O1", "O2", "O3"} {
		start := time.Now()
		if _, err := s.Send(ctx, fix2.NewBuilder(session.FIX44, "D").SetString(11, id)); err != nil {
			t.Fatal(err)
		}
		p.expect("D", "11", id)
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("%s written after %s", id, d)
		}
	}
}
