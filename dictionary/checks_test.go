package dictionary_test

import (
	"errors"
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
