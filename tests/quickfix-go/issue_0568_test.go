package quickfixgo

import (
	"bytes"
	"context"
	"slices"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/568
//
// A message with a repeating group, parsed from bytes and built again, came
// out with its body sorted by tag: 448|452|453 instead of 453|448|452. Built
// from its parsed fields, a message is the one parsed.
func TestIssue568_ParseThenBuildKeepsGroupOrder(t *testing.T) {
	orig := raw("8=FIX.4.4|35=D|34=1|49=S|52=20230622-08:23:27.847|56=T|11=1234|40=2|54=1|60=20230622-08:23:27.847|453=1|448=party-in-da-house|452=3")
	for name, p := range map[string]*fix2.Profile{"default": nil, "FIX44": dict(t, "FIX44").Profile()} {
		var m fix2.Message
		if err := fix2.ParseInto(&m, orig, p); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fields := m.Fields()
		b := fix2.NewBuilder(string(m.BeginString()), string(m.MsgType()))
		for _, f := range fields[3 : len(fields)-1] {
			b.Append(f.Tag, f.Value)
		}
		if got := b.Build(); !bytes.Equal(got, orig) {
			t.Errorf("%s: built %s, want %s", name, visible(got), visible(orig))
		}
	}
}

// The message, built in another process and passed as bytes, is sent on a
// session: its body goes out in the order it was parsed in.
func TestIssue568_ParsedMessageSentKeepsGroupOrder(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	orig := raw("8=FIX.4.4|35=D|11=1234|40=2|54=1|60=20230622-08:23:27.847|453=1|448=party-in-da-house|452=3")
	m, err := fix2.Parse(orig)
	if err != nil {
		t.Fatal(err)
	}
	fields := m.Fields()
	b := fix2.NewBuilder(session.FIX44, string(m.MsgType()))
	for _, f := range fields[3 : len(fields)-1] {
		b.Append(f.Tag, f.Value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := s.Send(ctx, b); err != nil {
		t.Fatal(err)
	}
	sent := p.expect("D").Fields()
	var got []byte
	for _, f := range sent[slices.IndexFunc(sent, func(f fix2.Field) bool { return f.Tag == 56 })+1 : len(sent)-1] {
		got = fix2.AppendTagValue(got, f.Tag, f.Value)
	}
	if want := "11=1234|40=2|54=1|60=20230622-08:23:27.847|453=1|448=party-in-da-house|452=3|"; visible(got) != want {
		t.Errorf("sent body %s, want %s", visible(got), want)
	}
}
