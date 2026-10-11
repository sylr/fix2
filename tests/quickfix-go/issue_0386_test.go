package quickfixgo

import (
	"context"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/386
//
// With a data dictionary on the session, a resent message was rebuilt from
// its parsed fields, sorted by tag: its repeating groups came out scrambled.
// A retransmission carries the body as it was first sent.
func TestIssue386_ResendKeepsGroupOrder(t *testing.T) {
	s, _, p := loggedOn(t, func(c *session.Config) { c.Dictionaries.Session = dict(t, "FIX44") })
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Group fields, and the fields after the group, out of tag order.
	order := fix2.NewBuilder(session.FIX44, "D").
		SetString(11, "O1").SetInt(453, 2).
		AppendString(448, "P1").AppendString(447, "D").AppendInt(452, 1).
		AppendString(448, "P2").AppendString(447, "D").AppendInt(452, 3).
		SetString(55, "EURUSD").SetString(54, "1").SetString(60, "20260410-12:00:00").SetString(40, "2")
	if _, err := s.Send(ctx, order); err != nil {
		t.Fatal(err)
	}
	orig := p.expect("D")
	p.send("2", 0, "7", "2", "16", "0")
	resent := p.expect("D", "34", "2", "43", "Y")
	if got, want := i386Body(resent), i386Body(orig); got != want {
		t.Errorf("resent body %s, want %s", got, want)
	}
}

// i386Body returns the fields of m after its standard header, in wire order.
func i386Body(m *fix2.Message) string {
	var b []byte
	for _, f := range m.Fields() {
		switch f.Tag {
		case 8, 9, 35, 34, 43, 49, 52, 56, 122, 10:
			continue
		}
		b = fix2.AppendTagValue(b, f.Tag, f.Value)
	}
	return visible(b)
}
