package quickfixgo

import (
	"context"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/276
//
// Session code unaware of a message's structure (no data dictionary) parsed
// stored messages flat and rebuilt them for resends: repeating groups, nested
// ones included, did not survive. A resend must not need to know the groups:
// the body goes out as it was first sent.
func TestIssue276_ResendWithoutDictionaryKeepsGroups(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// A NoPartyIDs group with nested NoPartySubIDs, then body fields.
	order := fix2.NewBuilder(session.FIX44, "D").
		SetString(11, "O1").SetInt(453, 2).
		AppendString(448, "P1").AppendString(447, "D").AppendInt(452, 1).
		AppendInt(802, 2).AppendString(523, "S1").AppendInt(803, 2).AppendString(523, "S2").AppendInt(803, 3).
		AppendString(448, "P2").AppendString(447, "D").AppendInt(452, 3).
		SetString(55, "EURUSD").SetString(54, "1").SetString(60, "20260410-12:00:00").SetString(40, "2")
	if _, err := s.Send(ctx, order); err != nil {
		t.Fatal(err)
	}
	orig := p.expect("D")
	p.send("2", 0, "7", "1", "16", "0")
	p.expect("4", "34", "1", "123", "Y", "36", "2")
	resent := p.expect("D", "34", "2", "43", "Y")
	if got, want := i276Body(resent), i276Body(orig); got != want {
		t.Errorf("resent body %s, want %s", got, want)
	}
}

// i276Body returns the fields of m after its standard header, in wire order.
func i276Body(m *fix2.Message) string {
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
