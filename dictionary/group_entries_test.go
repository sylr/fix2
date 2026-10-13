package dictionary_test

import (
	"errors"
	"strings"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
)

const groupEntriesXML = `<fix type="FIX" major="4" minor="4">
<header>
  <field name="MsgType" required="Y"/>
  <group name="NoHops" required="N"><field name="HopCompID" required="N"/><field name="HopRefID" required="Y"/></group>
</header>
<trailer/>
<messages>
<message name="List" msgtype="E" msgcat="app">
  <group name="NoOrders" required="Y">
    <field name="ClOrdID" required="Y"/>
    <field name="ListSeqNo" required="Y"/>
    <component name="Instrument" required="Y"/>
    <component name="Alloc" required="N"/>
    <field name="EncodedTextLen" required="N"/>
    <field name="EncodedText" required="N"/>
    <group name="NoLegs" required="Y"><field name="LegSymbol" required="N"/><field name="LegSide" required="Y"/><field name="LegQty" required="N"/></group>
    <field name="Side" required="Y"/>
  </group>
</message>
</messages>
<components>
  <component name="Instrument"><field name="Symbol" required="Y"/><field name="SecurityID" required="N"/></component>
  <component name="Alloc"><field name="AllocAccount" required="Y"/><field name="AllocQty" required="N"/></component>
</components>
<fields>
<field number="11" name="ClOrdID" type="STRING"/>
<field number="35" name="MsgType" type="STRING"/>
<field number="48" name="SecurityID" type="STRING"/>
<field number="54" name="Side" type="CHAR"/>
<field number="55" name="Symbol" type="STRING"/>
<field number="67" name="ListSeqNo" type="INT"/>
<field number="73" name="NoOrders" type="NUMINGROUP"/>
<field number="79" name="AllocAccount" type="STRING"/>
<field number="80" name="AllocQty" type="QTY"/>
<field number="354" name="EncodedTextLen" type="LENGTH"/>
<field number="355" name="EncodedText" type="DATA"/>
<field number="555" name="NoLegs" type="NUMINGROUP"/>
<field number="600" name="LegSymbol" type="STRING"/>
<field number="624" name="LegSide" type="CHAR"/>
<field number="627" name="NoHops" type="NUMINGROUP"/>
<field number="687" name="LegQty" type="QTY"/>
<field number="628" name="HopCompID" type="STRING"/>
<field number="630" name="HopRefID" type="SEQNUM"/>
</fields></fix>`

// CheckGroupRequired and CheckGroupOrder validate each group entry against
// its group's definition, as QuickFIX does: its required fields, nested
// groups and those of an optional component it holds, and its order.
// Validate does not, nor does CheckAll without them.
func TestValidate_GroupEntries(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(groupEntriesXML))
	if err != nil {
		t.Fatal(err)
	}
	const legs = "555=1|600=L|624=1|"
	for _, tc := range []struct {
		name   string
		body   string
		tag    int // 0 for valid
		reason dictionary.RejectReason
		check  dictionary.Checks // the check reporting it
		msg    string            // in the error text
	}{
		{name: "valid", body: "35=E|73=1|11=A|67=1|55=X|" + legs + "54=1|"},
		{name: "two valid entries", body: "35=E|73=2|11=A|67=1|55=X|" + legs + "54=1|11=B|67=2|55=Y|48=S|" + legs + "54=2|"},
		{"required field missing", "35=E|73=1|11=A|55=X|" + legs + "54=1|", 67, 1, dictionary.CheckGroupRequired,
			"required tag 67 (ListSeqNo) missing in entry 1 of group 73 (NoOrders) in message List"},
		{"required component's field missing", "35=E|73=1|11=A|67=1|" + legs + "54=1|", 55, 1, dictionary.CheckGroupRequired, ""},
		{"last required field missing", "35=E|73=1|11=A|67=1|55=X|" + legs, 54, 1, dictionary.CheckGroupRequired, ""},
		{"required nested group missing", "35=E|73=1|11=A|67=1|55=X|54=1|", 555, 1, dictionary.CheckGroupRequired, ""},
		{"nested entry's required field missing", "35=E|73=1|11=A|67=1|55=X|555=1|600=L|54=1|", 624, 1, dictionary.CheckGroupRequired,
			"in entry 1 of group 555 (NoLegs)"},
		{"second entry's required field missing", "35=E|73=2|11=A|67=1|55=X|" + legs + "54=1|11=B|55=Y|" + legs + "54=2|", 67, 1, dictionary.CheckGroupRequired,
			"in entry 2 of group 73"},
		{"optional component without its required field", "35=E|73=1|11=A|67=1|55=X|80=5|" + legs + "54=1|", 79, 1, dictionary.CheckGroupRequired,
			"for component Alloc"},
		{name: "optional component complete", body: "35=E|73=1|11=A|67=1|55=X|79=ACC|80=5|" + legs + "54=1|"},
		{"field out of order", "35=E|73=1|11=A|55=X|67=1|" + legs + "54=1|", 67, 15, dictionary.CheckGroupOrder,
			"tag 67 (ListSeqNo) out of order in entry 1 of group 73 (NoOrders) in message List"},
		{"nested group out of order", "35=E|73=1|11=A|67=1|55=X|54=1|" + legs, 555, 15, dictionary.CheckGroupOrder, ""},
		{"nested entry out of order", "35=E|73=1|11=A|67=1|55=X|555=1|600=L|687=5|624=1|54=1|", 624, 15, dictionary.CheckGroupOrder, ""},
		{name: "DATA field in order", body: "35=E|73=1|11=A|67=1|55=X|354=2|355=ab|" + legs + "54=1|"},
		{"DATA field out of order", "35=E|73=1|11=A|67=1|55=X|" + legs + "54=1|354=2|355=ab|", 354, 15, dictionary.CheckGroupOrder, ""},
		// Out of order first, as QuickFIX finds it while parsing.
		{"out of order and missing a field", "35=E|73=1|11=A|55=X|67=1|" + legs, 67, 15, dictionary.CheckGroupOrder, ""},
		{"header group entry's required field missing", "35=E|627=1|628=H|73=1|11=A|67=1|55=X|" + legs + "54=1|", 630, 1, dictionary.CheckGroupRequired,
			"in entry 1 of group 627 (NoHops)"},
		{name: "header group entry complete", body: "35=E|627=1|628=H|630=1|73=1|11=A|67=1|55=X|" + legs + "54=1|"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parseMsg(t, buildFIXMsg(strings.ReplaceAll(tc.body, "|", "\x01")))
			if err := d.Validate(&m); err != nil {
				t.Errorf("Validate: %v, want nil", err)
			}
			err := d.ValidateWith(&m, dictionary.CheckAll)
			if tc.tag == 0 {
				if err != nil {
					t.Errorf("CheckAll: %v, want nil", err)
				}
				return
			}
			var ve *dictionary.ValidationError
			if !errors.As(err, &ve) || ve.Tag != tc.tag || ve.Reason != tc.reason {
				t.Fatalf("CheckAll: %v, want tag %d, reason %d", err, tc.tag, tc.reason)
			}
			want := dictionary.ErrRequiredTag
			if tc.reason == dictionary.ReasonRepeatingGroupOutOfOrder {
				want = dictionary.ErrGroupOrder
			}
			if !errors.Is(err, want) {
				t.Errorf("CheckAll: %v, want %v", err, want)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("CheckAll: %q, want it to hold %q", err, tc.msg)
			}
			if err := d.ValidateWith(&m, tc.check); !errors.As(err, &ve) || ve.Tag != tc.tag {
				t.Errorf("its check alone: %v, want tag %d", err, tc.tag)
			}
			if tc.name == "out of order and missing a field" {
				return
			}
			if err := d.ValidateWith(&m, dictionary.CheckAll&^tc.check); err != nil {
				t.Errorf("CheckAll without its check: %v, want nil", err)
			}
		})
	}
}

// Tags a group does not define are not ordered, and do not end the entry
// when unknown tags are accepted.
func TestValidate_GroupEntryUnknownTag(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(groupEntriesXML))
	if err != nil {
		t.Fatal(err)
	}
	m := parseMsg(t, buildFIXMsg(strings.ReplaceAll("35=E|73=1|11=A|9999=z|67=1|55=X|555=1|600=L|624=1|54=1|", "|", "\x01")))
	accept := dictionary.CheckAll &^ (dictionary.CheckUnknownTags | dictionary.CheckUserDefinedTags)
	if err := d.ValidateWith(&m, accept); err != nil {
		t.Errorf("accepting unknown tags: %v, want nil", err)
	}
}

// Neither an optional component's required nested group nor a LENGTH field
// a DATA field outside the component brings along makes the component
// required; a LENGTH field declared after its DATA field still sits right
// before it.
func TestValidate_GroupEntryComponents(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/><messages>
<message name="E" msgtype="E" msgcat="app"><group name="G" required="N"><field name="D" required="N"/><component name="C" required="N"/><field name="RawData" required="N"/></group></message>
<message name="M" msgtype="M" msgcat="app"><field name="D" required="N"/><component name="C" required="N"/><field name="RawData" required="N"/></message>
<message name="N" msgtype="N" msgcat="app"><group name="G" required="N"><field name="D" required="N"/><component name="L" required="N"/></group></message>
<message name="R" msgtype="R" msgcat="app"><group name="NoR" required="N"><field name="RawData" required="N"/><field name="F" required="N"/><field name="RawDataLength" required="N"/></group></message>
</messages><components>
<component name="C"><field name="R" required="Y"/><field name="RawDataLength" required="N"/></component>
<component name="L"><field name="S" required="N"/><group name="NoN" required="Y"><field name="X" required="Y"/></group></component>
</components><fields>
<field number="35" name="MsgType" type="STRING"/><field number="95" name="RawDataLength" type="LENGTH"/><field number="96" name="RawData" type="DATA"/>
<field number="100" name="G" type="NUMINGROUP"/><field number="101" name="D" type="STRING"/><field number="102" name="R" type="STRING"/>
<field number="103" name="S" type="STRING"/><field number="104" name="F" type="STRING"/><field number="161" name="NoR" type="NUMINGROUP"/>
<field number="200" name="NoN" type="NUMINGROUP"/><field number="201" name="X" type="STRING"/>
</fields></fix>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		tag        int // 0 for valid
	}{
		{"entry: DATA outside the component", "35=E|100=1|101=d|95=2|96=ab|", 0},
		{"entry: component present", "35=E|100=1|101=d|102=r|", 0},
		{"entry: component absent", "35=E|100=1|101=d|", 0},
		{"message: DATA outside the component", "35=M|101=d|95=2|96=ab|", 0},
		{"entry: optional component's required group absent", "35=N|100=1|101=d|", 0},
		{"entry: optional component's required group missing", "35=N|100=1|101=d|103=s|", 200},
		{"entry: optional component's required group present", "35=N|100=1|101=d|103=s|200=1|201=x|", 0},
		{"entry: LENGTH declared after its DATA", "35=R|161=1|95=2|96=ab|104=f|", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m fix2.Message
			if err := fix2.ParseInto(&m, buildFIXMsg(strings.ReplaceAll(tc.body, "|", "\x01")), d.Profile()); err != nil {
				t.Fatal(err)
			}
			err := d.ValidateWith(&m, dictionary.CheckAll)
			var ve *dictionary.ValidationError
			switch {
			case tc.tag == 0 && err != nil:
				t.Errorf("CheckAll: %v, want nil", err)
			case tc.tag != 0 && (!errors.As(err, &ve) || ve.Tag != tc.tag || ve.Reason != dictionary.ReasonRequiredTagMissing):
				t.Errorf("CheckAll: %v, want required tag %d missing", err, tc.tag)
			}
		})
	}
}

// A DATA field whose LENGTH field the scope does not declare elsewhere
// still shows its component is present; a LENGTH-delimited entry whose
// DATA field the group declares last is in order with them adjacent, as
// generated builders write it; a group's count is reported before its
// entries' missing fields, as it comes first.
func TestValidate_GroupEntryDataAndOrder(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/><messages>
<message name="M" msgtype="M" msgcat="app"><component name="C" required="N"/></message>
<message name="E" msgtype="E" msgcat="app"><group name="G" required="N"><field name="D" required="N"/><component name="C" required="N"/></group></message>
<message name="L" msgtype="L" msgcat="app"><group name="G" required="N"><field name="RawDataLength" required="N"/><field name="D" required="N"/><field name="RawData" required="N"/></group></message>
</messages><components>
<component name="C"><field name="R" required="Y"/><field name="RawData" required="N"/></component>
</components><fields>
<field number="35" name="MsgType" type="STRING"/><field number="95" name="RawDataLength" type="LENGTH"/><field number="96" name="RawData" type="DATA"/>
<field number="100" name="G" type="NUMINGROUP"/><field number="101" name="D" type="STRING"/><field number="102" name="R" type="STRING"/>
</fields></fix>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		tag        int // 0 for valid
	}{
		{"message: DATA in the component, its LENGTH declared nowhere", "35=M|95=2|96=ab|", 102},
		{"entry: DATA in the component, its LENGTH declared nowhere", "35=E|100=1|101=d|95=2|96=ab|", 102},
		{"entry: component complete", "35=E|100=1|101=d|102=r|95=2|96=ab|", 0},
		{"entry: LENGTH first, DATA declared last", "35=L|100=1|95=2|96=ab|101=f|", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m fix2.Message
			if err := fix2.ParseInto(&m, buildFIXMsg(strings.ReplaceAll(tc.body, "|", "\x01")), d.Profile()); err != nil {
				t.Fatal(err)
			}
			err := d.ValidateWith(&m, dictionary.CheckAll)
			var ve *dictionary.ValidationError
			switch {
			case tc.tag == 0 && err != nil:
				t.Errorf("CheckAll: %v, want nil", err)
			case tc.tag != 0 && (!errors.As(err, &ve) || ve.Tag != tc.tag || ve.Reason != dictionary.ReasonRequiredTagMissing):
				t.Errorf("CheckAll: %v, want required tag %d missing", err, tc.tag)
			}
		})
	}

	g, err := xmldict.Load(strings.NewReader(groupEntriesXML))
	if err != nil {
		t.Fatal(err)
	}
	m := parseMsg(t, buildFIXMsg(strings.ReplaceAll("35=E|73=2|11=A|55=X|555=1|600=L|624=1|54=1|", "|", "\x01")))
	var ve *dictionary.ValidationError
	if err := g.ValidateWith(&m, dictionary.CheckAll); !errors.As(err, &ve) || ve.Tag != 73 || ve.Reason != dictionary.ReasonIncorrectNumInGroupCount {
		t.Errorf("count 2, one entry without its ListSeqNo: %v, want NumInGroup 73 incorrect", err)
	}
	if err := g.ValidateWith(&m, dictionary.CheckGroupRequired); !errors.As(err, &ve) || ve.Tag != 67 {
		t.Errorf("counts unchecked: %v, want required tag 67 missing", err)
	}
}
