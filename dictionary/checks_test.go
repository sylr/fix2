package dictionary_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
)

func TestValidateWith(t *testing.T) {
	d := loadTestDict(t)
	const header = "49=S\x0156=T\x0134=1\x0152=20231001-12:00:00\x01"
	const body = "11=ORD\x0154=1\x0160=20231001-12:00:00\x0140=1\x01"
	for _, tc := range []struct {
		name   string
		msg    string
		checks dictionary.Checks
		err    error
		tag    int
		reason dictionary.RejectReason
	}{
		{"valid", "35=D\x01" + header + body + "55=AAPL\x0178=1\x0179=A\x01", dictionary.CheckAll, nil, 0, 0},
		{"empty value", "35=D\x01" + header + body + "55=\x01", dictionary.CheckEmptyValues, dictionary.ErrEmptyValue, 55, dictionary.ReasonTagSpecifiedWithoutAValue},
		{"empty value unchecked", "35=D\x01" + header + body + "55=\x01", dictionary.CheckAll &^ dictionary.CheckEmptyValues, nil, 0, 0},
		{"empty enum value", "35=D\x01" + header + "11=ORD\x0154=\x0160=20231001-12:00:00\x0140=1\x0155=A\x01", dictionary.CheckAll, dictionary.ErrEmptyValue, 54, dictionary.ReasonTagSpecifiedWithoutAValue},
		{"bad SEQNUM", "35=D\x0149=S\x0156=T\x0134=x1\x0152=20231001-12:00:00\x01" + body + "55=A\x01", dictionary.CheckFormats, dictionary.ErrIncorrectFormat, 34, dictionary.ReasonIncorrectDataFormat},
		{"negative SEQNUM", "35=D\x0149=S\x0156=T\x0134=-1\x0152=20231001-12:00:00\x01" + body + "55=A\x01", dictionary.CheckFormats, dictionary.ErrIncorrectFormat, 34, dictionary.ReasonIncorrectDataFormat},
		{"bad UTCTIMESTAMP", "35=D\x01" + header + "11=ORD\x0154=1\x0160=20231001\x0140=1\x0155=A\x01", dictionary.CheckFormats, dictionary.ErrIncorrectFormat, 60, dictionary.ReasonIncorrectDataFormat},
		{"bad format unchecked", "35=D\x01" + header + "11=ORD\x0154=1\x0160=20231001\x0140=1\x0155=A\x01", 0, nil, 0, 0},
		{"repeated tag", "35=D\x01" + header + body + "55=A\x0155=B\x01", dictionary.CheckRepeatedTags, dictionary.ErrRepeatedTag, 55, dictionary.ReasonTagAppearsMoreThanOnce},
		{"repeated in group entries is fine", "35=D\x01" + header + body + "55=A\x0178=2\x0179=A\x0179=B\x01", dictionary.CheckAll, nil, 0, 0},
		{"header after body", "35=D\x0149=S\x0156=T\x0134=1\x01" + body + "52=20231001-12:00:00\x0155=A\x01", dictionary.CheckOrder, dictionary.ErrOutOfOrder, 52, dictionary.ReasonTagSpecifiedOutOfOrder},
		{"too few entries", "35=D\x01" + header + body + "55=A\x0178=2\x0179=A\x01", dictionary.CheckGroupCounts, dictionary.ErrGroupCount, 78, dictionary.ReasonIncorrectNumInGroupCount},
		{"count not a number", "35=D\x01" + header + body + "55=A\x0178=x\x0179=A\x01", dictionary.CheckGroupCounts, dictionary.ErrGroupCount, 78, dictionary.ReasonIncorrectNumInGroupCount},
		{"undefined tag", "35=D\x01" + header + body + "55=A\x014999=x\x01", dictionary.CheckUnknownTags, dictionary.ErrUnexpectedTag, 4999, dictionary.ReasonInvalidTagNumber},
		{"undefined tag accepted", "35=D\x01" + header + body + "55=A\x014999=x\x01", dictionary.CheckAll &^ dictionary.CheckUnknownTags, nil, 0, 0},
		{"tag of another message", "35=D\x01" + header + body + "55=A\x01112=x\x01", dictionary.CheckUnknownTags, dictionary.ErrUnexpectedTag, 112, dictionary.ReasonTagNotDefinedForMessageType},
		{"tag of another message accepted", "35=D\x01" + header + body + "55=A\x01112=x\x01", dictionary.CheckAll &^ dictionary.CheckUnknownTags, nil, 0, 0},
		{"unknown tag in the header accepted", "35=D\x014999=x\x01" + header + body + "55=A\x01", dictionary.CheckAll &^ dictionary.CheckUnknownTags, nil, 0, 0},
		{"user-defined tag not accepted with tags below 5000", "35=D\x01" + header + body + "55=A\x019001=x\x01", dictionary.CheckAll &^ dictionary.CheckUnknownTags, dictionary.ErrUnexpectedTag, 9001, dictionary.ReasonInvalidTagNumber},
		{"user-defined tag accepted", "35=D\x01" + header + body + "55=A\x019001=x\x01", dictionary.CheckAll &^ dictionary.CheckUserDefinedTags, nil, 0, 0},
		{"tag below 5000 not accepted with user-defined tags", "35=D\x01" + header + body + "55=A\x014999=x\x01", dictionary.CheckAll &^ dictionary.CheckUserDefinedTags, dictionary.ErrUnexpectedTag, 4999, dictionary.ReasonInvalidTagNumber},
		{"accepted unknown tag repeated", "35=D\x01" + header + body + "55=A\x019001=x\x019001=y\x01", dictionary.CheckAll &^ dictionary.CheckUserDefinedTags, dictionary.ErrRepeatedTag, 9001, dictionary.ReasonTagAppearsMoreThanOnce},
		{"rejected unknown tag repeated", "35=D\x01" + header + body + "55=A\x019001=x\x019001=y\x01", dictionary.CheckAll, dictionary.ErrUnexpectedTag, 9001, dictionary.ReasonInvalidTagNumber},
		{"unknown tag still checked for a value", "35=D\x01" + header + body + "55=A\x019001=\x01", dictionary.CheckAll &^ dictionary.CheckUserDefinedTags, dictionary.ErrEmptyValue, 9001, dictionary.ReasonTagSpecifiedWithoutAValue},
		// An entry keeps the unknown tags it holds: they neither end the
		// group nor make its other entries' tags repeat outside it.
		{"unknown tags in group entries accepted", "35=D\x01" + header + body + "55=A\x0178=2\x0179=A\x019001=x\x019001=y\x0179=B\x014999=z\x01", dictionary.CheckAll &^ (dictionary.CheckUnknownTags | dictionary.CheckUserDefinedTags), nil, 0, 0},
		{"unknown tag in a group entry reported", "35=D\x01" + header + body + "55=A\x0178=2\x0179=A\x019001=x\x0179=B\x01", dictionary.CheckAll, dictionary.ErrUnexpectedTag, 9001, dictionary.ReasonInvalidTagNumber},
		{"required tag after an unknown tag in a group entry", "35=D\x01" + header + body + "78=1\x0179=A\x019001=x\x0155=A\x01", dictionary.CheckAll &^ dictionary.CheckUserDefinedTags, nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := parseMsg(t, buildFIXMsg(tc.msg))
			err := d.ValidateWith(&msg, tc.checks)
			if tc.err == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *dictionary.ValidationError
			if !errors.Is(err, tc.err) || !errors.As(err, &ve) || ve.Tag != tc.tag || ve.Reason != tc.reason {
				t.Fatalf("got %v (%+v), want %v for tag %d, reason %d", err, ve, tc.err, tc.tag, tc.reason)
			}
		})
	}
	// Validate runs none of the extra checks.
	msg := parseMsg(t, buildFIXMsg("35=D\x01"+header+body+"55=A\x0155=B\x01"))
	if err := d.Validate(&msg); err != nil {
		t.Errorf("Validate with a repeated tag: %v", err)
	}
	// But it rejects unknown tags.
	for _, tag := range []string{"4999", "9001"} {
		msg := parseMsg(t, buildFIXMsg("35=D\x01"+header+body+"55=A\x01"+tag+"=x\x01"))
		if err := d.Validate(&msg); !errors.Is(err, dictionary.ErrUnexpectedTag) {
			t.Errorf("Validate with tag %s = %v, want ErrUnexpectedTag", tag, err)
		}
	}
}

func TestDefines(t *testing.T) {
	d := loadTestDict(t)
	for _, tc := range []struct {
		msgType string
		tag     int
		want    bool
	}{
		{"D", 11, true},   // the message's
		{"D", 79, true},   // a group entry's
		{"D", 49, true},   // the header's
		{"D", 10, true},   // the trailer's
		{"D", 112, false}, // another message's
		{"D", 4999, false},
		{"Z", 11, false}, // no such message
	} {
		if got := d.Defines(tc.msgType, tc.tag); got != tc.want {
			t.Errorf("Defines(%q, %d) = %v, want %v", tc.msgType, tc.tag, got, tc.want)
		}
	}
	for tag, want := range map[int]bool{11: true, 112: true, 49: true, 4999: false} {
		if got := d.DefinesAny(tag); got != want {
			t.Errorf("DefinesAny(%d) = %v, want %v", tag, got, want)
		}
	}
}

// The entries of the header's groups, which every message carries, keep the
// tags no message allows, those the dictionary declares but no message uses
// included; one some message allows ends them.
func TestValidateWith_UnknownTagsInHeaderGroups(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="4">
  <header>
    <field name="MsgType" required="Y"/>
    <group name="NoHops" required="N"><field name="HopCompID" required="N"/><field name="HopSendingTime" required="N"/></group>
  </header>
  <trailer/>
  <messages>
    <message name="Heartbeat" msgtype="0" msgcat="admin"><field name="TestReqID" required="N"/></message>
    <message name="TestRequest" msgtype="1" msgcat="admin"><field name="TestReqID" required="Y"/></message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="627" name="NoHops" type="NUMINGROUP"/>
    <field number="628" name="HopCompID" type="STRING"/>
    <field number="629" name="HopSendingTime" type="STRING"/>
    <field number="112" name="TestReqID" type="STRING"/>
    <field number="999" name="Unused" type="STRING"/>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	const accept = dictionary.CheckAll &^ (dictionary.CheckUnknownTags | dictionary.CheckUserDefinedTags)
	for _, tc := range []struct {
		name string
		hop  string // a field inside the first entry
		err  error
	}{
		{"declared, unused", "999=x", nil},
		{"undefined", "4999=x", nil},
		{"user-defined", "9001=x", nil},
		// TestReqID ends the group, whose next entry's tags then come after
		// the body.
		{"another message's", "112=x", dictionary.ErrOutOfOrder},
	} {
		msg := parseMsg(t, buildFIXMsg("35=0\x01627=2\x01628=A\x01"+tc.hop+"\x01629=t\x01628=B\x01629=t\x01"))
		if err := d.ValidateWith(&msg, accept); !errors.Is(err, tc.err) && (tc.err != nil || err != nil) {
			t.Errorf("%s: ValidateWith = %v, want %v", tc.name, err, tc.err)
		}
	}
}

func TestValidateWith_Formats(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="2">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <field name="I" required="N"/><field name="F" required="N"/><field name="C" required="N"/>
      <field name="B" required="N"/><field name="T" required="N"/><field name="D" required="N"/>
      <field name="Y" required="N"/><field name="S" required="N"/>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="5001" name="I" type="INT"/>
    <field number="5002" name="F" type="PRICE"/>
    <field number="5003" name="C" type="CHAR"/>
    <field number="5004" name="B" type="BOOLEAN"/>
    <field number="5005" name="T" type="UTCTIMEONLY"/>
    <field number="5006" name="D" type="LOCALMKTDATE"/>
    <field number="5007" name="Y" type="MONTHYEAR"/>
    <field number="5008" name="S" type="STRING"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field string
		ok    []string
		bad   []string
	}{
		{"5001", []string{"0", "-12", "007"}, []string{"-", "1.5", "1e3", "+1"}},
		{"5002", []string{"1", "-1.5", ".5", "5.", "00.10"}, []string{".", "-", "1.2.3", "1e3", "NaN"}},
		{"5003", []string{"x"}, []string{"xy"}},
		{"5004", []string{"Y", "N"}, []string{"y", "YES", "1"}},
		{"5005", []string{"12:00:00", "12:00:00.123"}, []string{"12:00", "25:00:00"}},
		{"5006", []string{"20231001"}, []string{"2023101", "20231301"}},
		{"5007", []string{"202310", "20231001", "202310w2"}, []string{"202313", "202300", "202310w6", "2023101"}},
		{"5008", []string{"anything at all"}, nil},
	} {
		for _, v := range append(tc.ok, tc.bad...) {
			want := !contains(tc.bad, v)
			msg := parseMsg(t, buildFIXMsg("35=M\x01"+tc.field+"="+v+"\x01"))
			if err := d.ValidateWith(&msg, dictionary.CheckFormats); (err == nil) != want {
				t.Errorf("tag %s value %q: err = %v, want valid %v", tc.field, v, err, want)
			}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Before FIX 4.2, CHAR fields held any string.
func TestValidateWith_CharBeforeFIX42(t *testing.T) {
	d, err := xmldict.LoadFile("../cmd/fixgen/testdata/FIX40.xml")
	if err != nil {
		t.Fatal(err)
	}
	if fd := d.FieldByName("Text"); fd == nil || fd.Type != "CHAR" {
		t.Fatalf("FIX40.xml Text = %+v, want a CHAR field", fd)
	}
	body := "35=3\x0149=S\x0156=T\x0134=1\x0152=20231001-12:00:00\x0145=1\x0158=a longer text\x01"
	raw := fix2.AppendTrailer([]byte("8=FIX.4.0\x019="+strconv.Itoa(len(body))+"\x01"+body), 0)
	m := parseMsg(t, raw)
	if err := d.ValidateWith(&m, dictionary.CheckFormats); err != nil {
		t.Errorf("FIX 4.0 Reject with a Text: %v", err)
	}
}

func TestValidateWith_StructureEdges(t *testing.T) {
	d := loadTestDict(t)
	const header = "49=S\x0156=T\x0134=1\x0152=20231001-12:00:00\x01"
	const body = "11=ORD\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=A\x01"
	for name, msg := range map[string]string{
		"more entries than the count": "35=D\x01" + header + body + "78=1\x0179=A\x0179=B\x01",
		"entries after a zero count":  "35=D\x01" + header + body + "78=0\x0179=A\x01",
	} {
		m := parseMsg(t, buildFIXMsg(msg))
		var ve *dictionary.ValidationError
		if err := d.ValidateWith(&m, dictionary.CheckGroupCounts); !errors.As(err, &ve) || ve.Reason != dictionary.ReasonIncorrectNumInGroupCount || ve.Tag != 78 {
			t.Errorf("%s: got %v, want an incorrect NumInGroup for 78", name, err)
		}
	}

	const xml = `<fix type="FIX" major="4" minor="4">
  <header>
    <field name="MsgType" required="Y"/><field name="SenderCompID" required="Y"/><field name="TargetCompID" required="Y"/>
  </header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <field name="SenderCompID" required="N"/>
      <field name="Symbol" required="N"/>
      <group name="NoOuter" required="N">
        <field name="Outer" required="N"/>
        <group name="NoInner" required="N"><field name="Inner" required="N"/></group>
      </group>
    </message>
    <message name="N" msgtype="N" msgcat="app">
      <group name="NoInner" required="N"><field name="Inner" required="N"/></group>
      <group name="NoOuter" required="N">
        <field name="Outer" required="N"/>
        <group name="NoInner" required="N"><field name="Inner" required="N"/></group>
      </group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="56" name="TargetCompID" type="STRING"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="100" name="NoOuter" type="NUMINGROUP"/>
    <field number="101" name="Outer" type="STRING"/>
    <field number="200" name="NoInner" type="NUMINGROUP"/>
    <field number="201" name="Inner" type="STRING"/>
  </fields>
</fix>`
	d2, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	for name, msg := range map[string]string{
		"a tag of both the header and the body":  "35=M\x0149=S\x0156=T\x0155=A\x01",
		"a nested counter also at the top level": "35=N\x0149=S\x0156=T\x01200=2\x01201=TOP1\x01201=TOP2\x01100=1\x01101=OUT\x01200=1\x01201=IN\x01",
		"nested groups within entries":           "35=M\x0149=S\x0156=T\x01100=2\x01101=A\x01200=2\x01201=X\x01201=Y\x01101=B\x01",
	} {
		m := parseMsg(t, buildFIXMsg(msg))
		if err := d2.ValidateWith(&m, dictionary.CheckAll); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	m := parseMsg(t, buildFIXMsg("35=M\x0149=S\x0156=T\x01100=1\x01101=A\x01200=2\x01201=X\x01"))
	var ve *dictionary.ValidationError
	if err := d2.ValidateWith(&m, dictionary.CheckGroupCounts); !errors.As(err, &ve) || ve.Tag != 200 {
		t.Errorf("nested count off: got %v, want an incorrect NumInGroup for 200", err)
	}
}

// FIX 4.0 timestamps have whole seconds.
func TestValidateWith_FIX40Time(t *testing.T) {
	d, err := xmldict.LoadFile("../cmd/fixgen/testdata/FIX40.xml")
	if err != nil {
		t.Fatal(err)
	}
	for v, ok := range map[string]bool{"20231001-12:00:00": true, "20231001-12:00:00.123": false} {
		body := "35=0\x0149=S\x0156=T\x0134=1\x0152=" + v + "\x01"
		m := parseMsg(t, fix2.AppendTrailer([]byte("8=FIX.4.0\x019="+strconv.Itoa(len(body))+"\x01"+body), 0))
		if err := d.ValidateWith(&m, dictionary.CheckFormats); (err == nil) != ok {
			t.Errorf("SendingTime %s: err = %v, want valid %v", v, err, ok)
		}
	}
}

// groupsXML defines message M, with an optional top-level D (101), R, where
// it is required, and P, without it. All three have the group NoG (100),
// whose entries are D, the delimiter, R (102), O (103) and the nested group
// NoN (110) of X (111), the delimiter, and Y (112).
const groupsXML = `<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/><messages>
<message name="M" msgtype="M" msgcat="app"><field name="D" required="N"/><component name="G" required="N"/></message>
<message name="R" msgtype="R" msgcat="app"><field name="D" required="Y"/><component name="G" required="N"/></message>
<message name="P" msgtype="P" msgcat="app"><component name="G" required="N"/></message>
</messages><components><component name="G">
  <group name="NoG" required="N">
    <field name="D" required="Y"/><field name="R" required="Y"/><field name="O" required="N"/>
    <group name="NoN" required="N"><field name="X" required="Y"/><field name="Y" required="N"/></group>
  </group>
</component></components><fields>
<field number="35" name="MsgType" type="STRING"/>
<field number="100" name="NoG" type="NUMINGROUP"/>
<field number="101" name="D" type="STRING"/>
<field number="102" name="R" type="STRING"/>
<field number="103" name="O" type="STRING"/>
<field number="110" name="NoN" type="NUMINGROUP"/>
<field number="111" name="X" type="STRING"/>
<field number="112" name="Y" type="STRING"/>
</fields></fix>`

// Validation reads groups as GroupsOf and GetTopLevel do: a group holds at
// most as many entries as its counter says, and a field after them that
// shares the delimiter's tag is the message's own.
func TestValidate_GroupBoundaries(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(groupsXML))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		entries    int    // as GroupsOf reads them
		top        string // GetTopLevel(101), "" for none
		skip       dictionary.Checks
	}{
		{"delimiter after the group", "35=M|100=1|101=A|102=R|101=TOP|", 1, "TOP", 0},
		{"delimiter after a nested group", "35=M|100=1|101=A|102=R|110=1|111=x|112=y|101=TOP|", 1, "TOP", 0},
		{"delimiter after a zero count", "35=M|100=0|101=TOP|", 0, "TOP", 0},
		{"delimiter before the group", "35=M|101=TOP|100=1|101=A|102=R|", 1, "TOP", 0},
		{"required delimiter after the group", "35=R|100=1|101=A|102=R|101=TOP|", 1, "TOP", 0},
		{"zero count", "35=M|100=0|", 0, "", 0},
		{"nested zero count", "35=M|100=1|101=A|102=R|110=0|", 1, "", 0},
		// TOP starts the second entry, which lacks R: CheckAll rejects it
		// (see TestValidate_GroupEntries).
		{"count 2, one entry then the delimiter", "35=M|100=2|101=A|102=R|101=TOP|", 2, "", dictionary.CheckGroupRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parseMsg(t, buildFIXMsg(strings.ReplaceAll(tc.body, "|", "\x01")))
			spec := d.CounterSpec(d.Messages[string(m.MsgType())], 100)
			if got := len(m.GroupsOf(spec)); got != tc.entries {
				t.Errorf("GroupsOf: %d entries, want %d", got, tc.entries)
			}
			if got := string(m.GetTopLevel(101, spec)); got != tc.top {
				t.Errorf("GetTopLevel(101) = %q, want %q", got, tc.top)
			}
			if err := d.Validate(&m); err != nil {
				t.Errorf("Validate: %v", err)
			}
			if err := d.ValidateWith(&m, dictionary.CheckAll&^tc.skip); err != nil {
				t.Errorf("ValidateWith(CheckAll&^%d): %v", tc.skip, err)
			}
		})
	}
	// A group entry's delimiter does not stand for the message's own.
	m := parseMsg(t, buildFIXMsg("35=R\x01100=1\x01101=A\x01102=R\x01"))
	for name, err := range map[string]error{"Validate": d.Validate(&m), "CheckAll": d.ValidateWith(&m, dictionary.CheckAll)} {
		var ve *dictionary.ValidationError
		if !errors.As(err, &ve) || ve.Tag != 101 || ve.Reason != dictionary.ReasonRequiredTagMissing {
			t.Errorf("%s without the top-level delimiter: %v, want required tag 101 missing", name, err)
		}
	}
}

// A tag a message holds only in group entries is not allowed outside them
// (reason 2), whether no counter announces it or its group has more entries
// than its counter says: that is reported as such (reason 16) when checked,
// if the next entry's delimiter cannot be the message's own. Accepting
// unknown tags accepts such a tag too, as QuickFIX/J does: group counts are
// then reported by CheckGroupCounts alone.
func TestValidate_GroupFieldOutsideEntries(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(groupsXML))
	if err != nil {
		t.Fatal(err)
	}
	const accept = dictionary.CheckAll &^ (dictionary.CheckUnknownTags | dictionary.CheckUserDefinedTags)
	type verdict struct {
		tag    int // 0 for none
		reason dictionary.RejectReason
	}
	for _, tc := range []struct {
		name                      string
		body                      string
		validate, all, acceptTags verdict // by Validate, CheckAll, and CheckAll accepting unknown tags
	}{
		{"group field without its counter", "35=P|102=R|", verdict{102, 2}, verdict{102, 2}, verdict{}},
		{"group field without its counter beside D", "35=M|102=R|", verdict{102, 2}, verdict{102, 2}, verdict{}},
		{"group delimiter without its counter", "35=P|101=A|", verdict{101, 2}, verdict{101, 2}, verdict{}},
		{"nested group field without its counter", "35=P|111=x|", verdict{111, 2}, verdict{111, 2}, verdict{}},
		{"group field after the group", "35=P|100=1|101=A|102=R|103=O|102=S|", verdict{102, 2}, verdict{102, 2}, verdict{}},
		// A repeated tag ends an entry, as GroupsOf reads it.
		{"field repeated in an entry", "35=P|100=1|101=A|102=R|103=X|103=Y|", verdict{103, 2}, verdict{103, 2}, verdict{}},
		{"delimiter not first", "35=P|100=1|102=R|101=A|", verdict{102, 2}, verdict{100, 16}, verdict{100, 16}},
		{"count 1, two entries", "35=P|100=1|101=A|102=R|101=B|102=S|", verdict{101, 2}, verdict{100, 16}, verdict{100, 16}},
		// D may be the message's own, not R after it.
		{"count 1, two entries, delimiter also top level", "35=M|100=1|101=A|102=R|101=B|102=S|", verdict{102, 2}, verdict{102, 2}, verdict{}},
		{"count 1, two nested entries", "35=P|100=1|101=A|102=R|110=1|111=x|111=y|", verdict{111, 2}, verdict{110, 16}, verdict{110, 16}},
		{"count 0, one entry", "35=P|100=0|101=A|102=R|", verdict{101, 2}, verdict{100, 16}, verdict{100, 16}},
		{"count not a number", "35=P|100=x|101=A|", verdict{101, 2}, verdict{100, 6}, verdict{100, 6}},
		// Unchanged.
		{"count 2, one entry", "35=P|100=2|101=A|102=R|", verdict{}, verdict{100, 16}, verdict{100, 16}},
		{"nested count 2, one entry", "35=M|100=1|101=A|102=R|110=2|111=x|", verdict{}, verdict{110, 16}, verdict{110, 16}},
		{"unknown tags", "35=P|4999=x|9001=y|", verdict{4999, 0}, verdict{4999, 0}, verdict{}},
		{"unknown tags in an entry", "35=P|100=1|101=A|4999=x|102=R|9001=y|", verdict{4999, 0}, verdict{4999, 0}, verdict{}},
		{"second entry without its required R", "35=P|100=2|101=A|102=R|110=1|111=x|101=B|", verdict{}, verdict{102, 1}, verdict{102, 1}},
		{"valid", "35=P|100=2|101=A|102=R|110=1|111=x|101=B|102=S|", verdict{}, verdict{}, verdict{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parseMsg(t, buildFIXMsg(strings.ReplaceAll(tc.body, "|", "\x01")))
			for _, c := range []struct {
				name string
				err  error
				want verdict
			}{
				{"Validate", d.Validate(&m), tc.validate},
				{"CheckAll", d.ValidateWith(&m, dictionary.CheckAll), tc.all},
				{"CheckAll accepting unknown tags", d.ValidateWith(&m, accept), tc.acceptTags},
			} {
				var ve *dictionary.ValidationError
				switch {
				case c.want.tag == 0 && c.err != nil:
					t.Errorf("%s: %v, want nil", c.name, c.err)
				case c.want.tag != 0 && (!errors.As(c.err, &ve) || ve.Tag != c.want.tag || ve.Reason != c.want.reason):
					t.Errorf("%s: %v, want tag %d, reason %d", c.name, c.err, c.want.tag, c.want.reason)
				case c.want.reason == dictionary.ReasonTagNotDefinedForMessageType && !errors.Is(c.err, dictionary.ErrTagOutsideGroup):
					t.Errorf("%s: %v, want ErrTagOutsideGroup", c.name, c.err)
				}
			}
		})
	}
	// Without CheckGroupCounts, the stray entry is accepted with unknown tags.
	m := parseMsg(t, buildFIXMsg("35=P\x01100=1\x01101=A\x01102=R\x01101=B\x01102=S\x01"))
	if err := d.ValidateWith(&m, accept&^dictionary.CheckGroupCounts); err != nil {
		t.Errorf("count 1, two entries, accepting unknown tags, counts unchecked: %v", err)
	}
}

// A nested group's entries end at its count, and the field after them that
// shares their delimiter's tag goes to the first enclosing entry, or the
// message, that may still hold it, as GroupsOf reads it: it is another entry
// only if none may.
func TestValidate_NestedGroupBoundaries(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/><messages>
<message name="Top" msgtype="U1" msgcat="app"><field name="Text" required="N"/><component name="P" required="N"/></message>
<message name="Grandparent" msgtype="U2" msgcat="app">
  <group name="NoQ" required="N"><field name="QID" required="N"/><component name="P" required="N"/><field name="Text" required="N"/></group>
</message>
<message name="Parent" msgtype="U3" msgcat="app">
  <group name="NoP" required="N"><field name="PID" required="N"/><field name="Text" required="N"/><group name="NoG" required="N"><field name="Text" required="N"/></group></group>
</message>
</messages><components><component name="P">
  <group name="NoP" required="N"><field name="PID" required="N"/><group name="NoG" required="N"><field name="Text" required="N"/></group></group>
</component></components><fields>
<field number="35" name="MsgType" type="STRING"/>
<field number="58" name="Text" type="STRING"/>
<field number="5000" name="NoP" type="NUMINGROUP"/>
<field number="5001" name="PID" type="STRING"/>
<field number="5002" name="NoG" type="NUMINGROUP"/>
<field number="5003" name="NoQ" type="NUMINGROUP"/>
<field number="5004" name="QID" type="STRING"/>
</fields></fix>`))
	if err != nil {
		t.Fatal(err)
	}
	const counts = dictionary.CheckGroupCounts | dictionary.CheckUnknownTags | dictionary.CheckUserDefinedTags
	parse := func(body string) fix2.Message {
		return parseMsg(t, buildFIXMsg(strings.ReplaceAll(body, "|", "\x01")))
	}

	// The message's own Text after the nested group.
	m := parse("35=U1|5000=1|5001=a|5002=1|58=g|58=top|")
	p := d.CounterSpec(d.Messages["U1"], 5000)
	if got := len(m.GroupsOf(p)); got != 1 {
		t.Errorf("U1: GroupsOf(NoP) = %d entries, want 1", got)
	}
	if got := string(m.GetTopLevel(58, p)); got != "top" {
		t.Errorf("U1: GetTopLevel(58) = %q, want top", got)
	}
	// The grandparent entry's Text after the nested group.
	m2 := parse("35=U2|5003=1|5004=q|5000=1|5001=p|5002=1|58=g|58=qtext|")
	q := d.CounterSpec(d.Messages["U2"], 5003)
	qs := m2.GroupsOf(q)
	if len(qs) != 1 {
		t.Fatalf("U2: GroupsOf(NoQ) = %d entries, want 1", len(qs))
	}
	if got := string(qs[0].GetTopLevel(58, q.Child(5000))); got != "qtext" {
		t.Errorf("U2: Q entry's GetTopLevel(58) = %q, want qtext", got)
	}
	for name, msg := range map[string]*fix2.Message{"U1": &m, "U2": &m2} {
		for _, c := range []struct {
			name string
			err  error
		}{
			{"Validate", d.Validate(msg)},
			{"counts", d.ValidateWith(msg, counts)},
			{"CheckAll", d.ValidateWith(msg, dictionary.CheckAll)},
		} {
			if c.err != nil {
				t.Errorf("%s: %s: %v", name, c.name, c.err)
			}
		}
	}

	// The parent entry holds Text already: the next Text is another entry
	// of the nested group, which Validate finds outside it.
	m3 := parse("35=U3|5000=1|5001=p|58=pt|5002=1|58=g|58=extra|")
	var ve *dictionary.ValidationError
	if err := d.ValidateWith(&m3, counts); !errors.As(err, &ve) || ve.Tag != 5002 || ve.Reason != dictionary.ReasonIncorrectNumInGroupCount {
		t.Errorf("U3: counts: %v, want an incorrect NumInGroup for 5002", err)
	}
	if err := d.Validate(&m3); !errors.As(err, &ve) || ve.Tag != 58 || ve.Reason != dictionary.ReasonTagNotDefinedForMessageType {
		t.Errorf("U3: Validate: %v, want 58 not defined for the message type", err)
	}
	// Without it, the next Text is the parent entry's, out of its order.
	m4 := parse("35=U3|5000=1|5001=p|5002=1|58=g|58=pt|")
	if err := d.ValidateWith(&m4, dictionary.CheckAll&^dictionary.CheckGroupOrder); err != nil {
		t.Errorf("U3: Text after the nested group: %v", err)
	}
	if err := d.ValidateWith(&m4, dictionary.CheckAll); !errors.As(err, &ve) || ve.Tag != 58 || ve.Reason != dictionary.ReasonRepeatingGroupOutOfOrder {
		t.Errorf("U3: Text after the nested group: %v, want 58 out of order", err)
	}
}

// The standard dictionaries allow outside group entries every tag a message
// requires and the counters of its groups, and every other tag of the
// message outside them or in its groups' entries.
func TestValidate_RealDictionariesTopLevel(t *testing.T) {
	files, err := filepath.Glob("../cmd/fixgen/testdata/*.xml")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files,
		"../tests/benchmarks/spec/FIX44.xml",
		"../contribs/coinbasemd/internal/fixt/FIXT11-prod-sand.xml",
		"../contribs/coinbasemd/internal/md/FIX50-prod-sand.xml",
		"../contribs/binancemd/internal/spotmd/spot-fix-md.xml",
	)
	if dir := os.Getenv("FIX2_QUICKFIX_TESTS"); dir != "" {
		spec, err := filepath.Glob(filepath.Join(dir, "..", "spec", "*.xml"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, spec...)
	}
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			d, err := xmldict.LoadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			grouped, groups := 0, false
			for _, m := range d.Messages {
				groups = groups || len(m.Groups) > 0
				for _, tag := range m.RequiredTags() {
					if !d.TopLevel(m, tag) {
						t.Errorf("%s: required tag %d not allowed at the top level", m.Name, tag)
					}
				}
				held := map[int]bool{}
				var walk func(groups []dictionary.GroupDef, nested bool)
				walk = func(groups []dictionary.GroupDef, nested bool) {
					for _, gd := range groups {
						if !nested && !d.TopLevel(m, gd.CounterTag) {
							t.Errorf("%s: counter %d not allowed at the top level", m.Name, gd.CounterTag)
						}
						held[gd.CounterTag] = held[gd.CounterTag] || nested
						for _, ref := range gd.Fields {
							held[ref.Tag] = true
						}
						walk(gd.Groups, true)
					}
				}
				walk(m.Groups, false)
				for _, ref := range m.Fields {
					switch {
					case !d.TopLevel(m, ref.Tag) && !held[ref.Tag]:
						t.Errorf("%s: tag %d allowed neither at the top level nor in a group entry", m.Name, ref.Tag)
					case !d.TopLevel(m, ref.Tag):
						grouped++
					}
				}
			}
			if groups && grouped == 0 {
				t.Error("no tag allowed only in group entries")
			}
		})
	}
}
