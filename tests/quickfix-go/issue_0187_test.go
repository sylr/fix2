package quickfixgo

import (
	"context"
	"slices"
	"testing"
)

// https://github.com/quickfixgo/quickfix/issues/187
//
// Once our Logout was sent, the messages received before its answer were
// dropped. They are in sequence and must be delivered.
func TestIssue187_MessagesBeforeLogoutAnswerDelivered(t *testing.T) {
	s, rec, p := loggedOn(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan error)
	go func() { done <- s.Logout(ctx, "bye") }()
	p.expect("5")
	p.send("8", 0, "11", "O1")
	p.send("8", 0, "11", "O2")
	p.send("5", 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "OnLogout", func() bool { return len(rec.logouts) == 1 })
	if got := rec.appValues(11); !slices.Equal(got, []string{"O1", "O2"}) {
		t.Errorf("delivered %q, want [O1 O2]", got)
	}
	if next := s.NextTargetSeqNum(); next != 5 {
		t.Errorf("NextTargetSeqNum = %d, want 5", next)
	}
}
