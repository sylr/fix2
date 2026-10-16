package quickfixgo

import (
	"bytes"
	"context"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// https://github.com/quickfixgo/quickfix/issues/761
//
// With a data dictionary, parsing a stored message whose body ends with a
// repeating group left its CheckSum in the body: the resend wrote it, then a
// fresh one, two 10= fields the counterparty rejected. A resent message must
// end with a single CheckSum.
func TestIssue761_ResendWithGroupHasOneCheckSum(t *testing.T) {
	d := dict(t, "FIX44")
	s, _, p := loggedOn(t, func(c *session.Config) { c.Dictionaries.Session = d })
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := s.Send(ctx, i761Order()); err != nil {
		t.Fatal(err)
	}
	orig := p.next()
	p.send("2", 0, "7", "2", "16", "0")
	resent := p.next()
	if n := bytes.Count(resent, []byte("\x0110=")); n != 1 {
		t.Fatalf("resent %s: %d CheckSums, want 1", visible(resent), n)
	}
	m, err := fix.Parse(resent)
	if err != nil {
		t.Fatalf("resent %s: %v", visible(resent), err)
	}
	if string(m.Get(43)) != "Y" || string(m.Get(34)) != "2" {
		t.Fatalf("resent %s, want message 2 with PossDupFlag", visible(resent))
	}
	o, _ := fix.Parse(orig)
	if got, want := i761Body(&m), i761Body(&o); got != want {
		t.Errorf("resent body %s, want %s", got, want)
	}
}

// The same, at the level of the rewrite: a stored message ending with a
// group, parsed with the dictionary's profile.
func TestIssue761_RewriteForResendWithGroupHasOneCheckSum(t *testing.T) {
	stored := raw("8=FIX.4.4|35=D|34=1|49=S|52=20260410-12:00:00|56=T|11=X|54=1|40=2|60=20260410-12:00:00|453=1|448=A|447=D|452=1")
	out, err := fix.RewriteForResend(nil, stored, dict(t, "FIX44").Profile(), []byte("20260410-12:00:01"))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(out, []byte("\x0110=")); n != 1 {
		t.Fatalf("rewritten %s: %d CheckSums, want 1", visible(out), n)
	}
	m, err := fix.Parse(out)
	if err != nil {
		t.Fatalf("rewritten %s: %v", visible(out), err)
	}
	o, _ := fix.Parse(stored)
	if got, want := i761Body(&m), i761Body(&o); got != want {
		t.Errorf("rewritten body %s, want %s", got, want)
	}
}

// i761Order is a NewOrderSingle whose body ends with a NoPartyIDs group.
func i761Order() *fix.Builder {
	return fix.NewBuilder(session.FIX44, "D").
		SetString(11, "O1").SetString(54, "1").SetString(40, "2").SetString(60, "20260410-12:00:00").
		SetInt(453, 2).
		AppendString(448, "P1").AppendString(447, "D").AppendInt(452, 1).
		AppendString(448, "P2").AppendString(447, "D").AppendInt(452, 3)
}

// i761Body returns the fields of m after its standard header, in wire order.
func i761Body(m *fix.Message) string {
	var b []byte
	for _, f := range m.Fields() {
		switch f.Tag {
		case 8, 9, 35, 34, 43, 49, 52, 56, 122, 10:
			continue
		}
		b = fix.AppendTagValue(b, f.Tag, f.Value)
	}
	return visible(b)
}
