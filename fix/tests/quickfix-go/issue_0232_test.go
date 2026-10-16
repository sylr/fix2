package quickfixgo

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/232
//
// Messages resent at the counterparty's request bypassed the application:
// it could neither see nor veto them. fix2 asks OnResend about every stored
// application message it is about to resend, false gap-filling it instead;
// ToAdmin is for Logon and Logout only, by design.
func TestIssue232_ResendAsksTheHandler(t *testing.T) {
	var h *i232Handler
	s, _, p := loggedOn(t, func(c *session.Config) {
		h = &i232Handler{recorder: c.Handler.(*recorder), veto: "O2"}
		c.Handler = h
	})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, id := range []string{"O1", "O2", "O3"} {
		if _, err := s.Send(ctx, fix.NewBuilder(session.FIX44, "D").SetString(11, id)); err != nil {
			t.Fatal(err)
		}
		p.expect("D", "11", id)
	}
	p.send("2", 0, "7", "1", "16", "0")
	p.expect("4", "34", "1", "123", "Y", "36", "2") // our Logon
	p.expect("D", "34", "2", "43", "Y", "11", "O1")
	p.expect("4", "34", "3", "43", "Y", "123", "Y", "36", "4") // vetoed
	p.expect("D", "34", "4", "43", "Y", "11", "O3")
	h.mu.Lock()
	defer h.mu.Unlock()
	if want := []string{"2 O1", "3 O2", "4 O3"}; !slices.Equal(h.asked, want) {
		t.Errorf("OnResend asked about %q, want %q", h.asked, want)
	}
}

// i232Handler records what OnResend is asked about and vetoes the order
// whose ClOrdID is veto.
type i232Handler struct {
	*recorder
	veto  string
	mu    sync.Mutex
	asked []string
}

func (h *i232Handler) OnResend(_ context.Context, _ *session.Session, seq uint64, m *fix.Message) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := string(m.Get(11))
	h.asked = append(h.asked, fmt.Sprintf("%d %s", seq, id))
	return id != h.veto
}
