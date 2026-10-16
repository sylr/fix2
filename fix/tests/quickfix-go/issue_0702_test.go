package quickfixgo

import (
	"testing"

	"github.com/sylr/fix2/fix"
)

// https://github.com/quickfixgo/quickfix/issues/702
//
// Body.Set on a parsed message changed its field map, but String still
// returned the raw bytes it was parsed from: the change was silently lost. A
// fix2 Message is read-only, so a parsed message is changed through a
// Builder holding its fields: the message it builds must carry the change.
func TestIssue702_SetOnParsedMessageReflectedWhenSerialized(t *testing.T) {
	m, err := fix.Parse(raw("8=FIX.4.4|35=D|34=2|49=A|52=20240101-00:00:00.000|56=B|1=OLD|11=1|55=SYM|54=1|38=100|40=1"))
	if err != nil {
		t.Fatal(err)
	}
	b := fix.NewBuilder(string(m.BeginString()), string(m.MsgType()))
	for _, f := range m.Fields()[3 : m.Count()-1] { // after 8, 9 and 35, before 10
		b.Append(f.Tag, f.Value)
	}
	b.SetString(1, "123456")

	out, err := fix.Parse(b.Build())
	if err != nil {
		t.Fatal(err)
	}
	var accounts []string
	for _, f := range out.Fields() {
		if f.Tag == 1 {
			accounts = append(accounts, string(f.Value))
		}
	}
	if len(accounts) != 1 || accounts[0] != "123456" {
		t.Errorf("Account %q, want [123456]", accounts)
	}
	if out.Count() != m.Count() || string(out.Get(11)) != "1" {
		t.Errorf("got %v, want the fields of %v", out.Fields(), m.Fields())
	}
}
