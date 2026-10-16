package quickfixgo

import (
	"errors"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
)

// https://github.com/quickfixgo/quickfix/issues/103
//
// Unmarshaling the MarketDataRequest of #102 failed with Incorrect data
// format for value, NoRelatedSym (146): its entry does not start with the
// group's first field, Symbol (55). With it, both groups must be read; without
// it, the error must say the group is malformed.
func TestIssue103_MarketDataRequestGroupsRead(t *testing.T) {
	m, err := fix.Parse(raw("8=FIXT.1.1|35=V|34=2|49=MDC|52=20160419-22:58:50.947|56=KMD|262=req_A|263=0|264=5|" +
		"146=1|55=DORZ17|48=DORZ17|22=99|267=3|269=0|269=1|269=2"))
	if err != nil {
		t.Fatal(err)
	}
	syms := &fix.GroupSpec{Counter: 146, Delim: 55, Tags: []int{55, 48, 22}}
	types := &fix.GroupSpec{Counter: 267, Delim: 269, Tags: []int{269}}
	if s := m.GroupsOf(syms, types); len(s) != 1 || string(s[0].Get(48)) != "DORZ17" || string(s[0].Get(22)) != "99" {
		t.Fatalf("got %d NoRelatedSym entries, want DORZ17", len(s))
	}
	var got []string
	for _, e := range m.GroupsOf(types, syms) {
		got = append(got, string(e.Get(269)))
	}
	if len(got) != 3 || got[0] != "0" || got[1] != "1" || got[2] != "2" {
		t.Fatalf("got MDEntryTypes %q, want 0, 1, 2", got)
	}
}

func TestIssue103_EntryWithoutDelimiterReported(t *testing.T) {
	d := dictionary.Compose(dict(t, "FIXT11"), dictXML(t, i103FIX50SP1))
	// The issue's message.
	m, err := fix.Parse(raw("8=FIXT.1.1|35=V|34=2|49=MDC|52=20160419-22:58:50.947|56=KMD|262=req_A|263=0|264=5|" +
		"146=1|48=DORZ17|22=99|267=3|269=0|269=1|269=2"))
	if err != nil {
		t.Fatal(err)
	}
	err = d.ValidateWith(&m, dictionary.CheckAll)
	// Repeating group fields out of order or Incorrect NumInGroup count:
	// either says the entry is not one.
	var ve *dictionary.ValidationError
	if !errors.As(err, &ve) || ve.Tag != 146 ||
		ve.Reason != dictionary.ReasonRepeatingGroupOutOfOrder && ve.Reason != dictionary.ReasonIncorrectNumInGroupCount {
		t.Fatalf("got %v, want NoRelatedSym (146) reported malformed", err)
	}
}

// i103FIX50SP1 is FIX 5.0 SP1's MarketDataRequest cut down to the fields of
// the message.
const i103FIX50SP1 = `<fix type="FIX" major="5" minor="0" servicepack="1">
  <header/>
  <trailer/>
  <messages>
    <message name="MarketDataRequest" msgtype="V" msgcat="app">
      <field name="MDReqID" required="Y"/>
      <field name="SubscriptionRequestType" required="Y"/>
      <field name="MarketDepth" required="Y"/>
      <component name="MDReqGrp" required="Y"/>
      <component name="InstrmtMDReqGrp" required="Y"/>
    </message>
  </messages>
  <components>
    <component name="MDReqGrp">
      <group name="NoMDEntryTypes" required="Y">
        <field name="MDEntryType" required="Y"/>
      </group>
    </component>
    <component name="InstrmtMDReqGrp">
      <group name="NoRelatedSym" required="Y">
        <component name="Instrument" required="Y"/>
      </group>
    </component>
    <component name="Instrument">
      <field name="Symbol" required="N"/>
      <field name="SecurityID" required="N"/>
      <field name="SecurityIDSource" required="N"/>
    </component>
  </components>
  <fields>
    <field number="22" name="SecurityIDSource" type="STRING"/>
    <field number="48" name="SecurityID" type="STRING"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="146" name="NoRelatedSym" type="NUMINGROUP"/>
    <field number="262" name="MDReqID" type="STRING"/>
    <field number="263" name="SubscriptionRequestType" type="CHAR"/>
    <field number="264" name="MarketDepth" type="INT"/>
    <field number="267" name="NoMDEntryTypes" type="NUMINGROUP"/>
    <field number="269" name="MDEntryType" type="CHAR"/>
  </fields>
</fix>`
