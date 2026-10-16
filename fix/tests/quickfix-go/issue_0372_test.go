package quickfixgo

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/372
//
// A clone of a received message shared its field bytes with the pooled buffer
// the next message was read into: handing it to another goroutine raced with
// the session. A clone must stay intact, and be race-free, while the session
// goes on receiving.
func TestIssue372_CloneHandedToGoroutineWhileSessionReceives(t *testing.T) {
	const n = 200
	h := &i372Handler{msgs: make(chan *fix.Message, n)}
	_, _, l := startAcceptor(t, func(c *session.Config) { c.Handler = h })
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A")
	kept := make(chan []*fix.Message)
	go func() {
		var ms []*fix.Message
		for i := range n {
			m := <-h.msgs
			// Read while the session reads the next messages.
			if err := i372Check(m, i); err != nil {
				t.Error(err)
			}
			ms = append(ms, m)
		}
		kept <- ms
	}()
	for i := range n {
		id := strconv.Itoa(i)
		p.send("D", 0, "11", id, "55", "SYM"+id, "54", "1", "38", "100", "40", "1", "58", "order "+id)
	}
	// Read again once the session has read them all.
	select {
	case ms := <-kept:
		for i, m := range ms {
			if err := i372Check(m, i); err != nil {
				t.Fatal(err)
			}
		}
	case <-time.After(timeout):
		t.Fatal("timed out waiting for the orders")
	}
}

// i372Handler hands a clone of every application message to another
// goroutine.
type i372Handler struct {
	session.BaseHandler
	msgs chan *fix.Message
}

func (h *i372Handler) FromApp(_ context.Context, _ *session.Session, m *fix.Message, _ session.Inbound) error {
	h.msgs <- m.Clone()
	return nil
}

// i372Check checks m is the i-th order sent.
func i372Check(m *fix.Message, i int) error {
	id := strconv.Itoa(i)
	if string(m.Get(11)) != id || string(m.Get(55)) != "SYM"+id || string(m.Get(58)) != "order "+id {
		return fmt.Errorf("message %d changed: 11=%s 55=%s 58=%s", i, m.Get(11), m.Get(55), m.Get(58))
	}
	return nil
}
