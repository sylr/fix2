package dictionary_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
)

const testDictXML = `<?xml version="1.0" encoding="UTF-8"?>
<fix type="FIX" major="4" minor="4" servicepack="0">
  <header>
    <field name="BeginString" required="Y"/>
    <field name="BodyLength" required="Y"/>
    <field name="MsgType" required="Y"/>
    <field name="SenderCompID" required="Y"/>
    <field name="TargetCompID" required="Y"/>
    <field name="MsgSeqNum" required="Y"/>
    <field name="SendingTime" required="Y"/>
  </header>
  <trailer>
    <field name="CheckSum" required="Y"/>
  </trailer>
  <messages>
    <message name="Heartbeat" msgtype="0" msgcat="admin">
      <field name="TestReqID" required="N"/>
    </message>
    <message name="NewOrderSingle" msgtype="D" msgcat="app">
      <field name="ClOrdID" required="Y"/>
      <field name="Side" required="Y"/>
      <field name="TransactTime" required="Y"/>
      <field name="OrdType" required="Y"/>
      <component name="Instrument" required="Y"/>
      <group name="NoAllocs" required="N">
        <field name="AllocAccount" required="N"/>
      </group>
    </message>
  </messages>
  <components>
    <component name="Instrument">
      <field name="Symbol" required="Y"/>
    </component>
  </components>
  <fields>
    <field number="8" name="BeginString" type="STRING"/>
    <field number="9" name="BodyLength" type="LENGTH"/>
    <field number="10" name="CheckSum" type="STRING"/>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="56" name="TargetCompID" type="STRING"/>
    <field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/>
    <field number="112" name="TestReqID" type="STRING"/>
    <field number="11" name="ClOrdID" type="STRING"/>
    <field number="54" name="Side" type="CHAR">
      <value enum="1" description="BUY"/>
      <value enum="2" description="SELL"/>
    </field>
    <field number="60" name="TransactTime" type="UTCTIMESTAMP"/>
    <field number="40" name="OrdType" type="CHAR">
      <value enum="1" description="MARKET"/>
      <value enum="2" description="LIMIT"/>
    </field>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="78" name="NoAllocs" type="NUMINGROUP"/>
    <field number="79" name="AllocAccount" type="STRING"/>
  </fields>
</fix>`

func loadTestDict(t *testing.T) *dictionary.Dict {
	t.Helper()
	d, err := xmldict.Load(strings.NewReader(testDictXML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return d
}

// buildFIXMsg constructs a valid FIX message from body fields for testing.
func buildFIXMsg(body string) []byte {
	bodyLen := len(body)
	prefix := fmt.Sprintf("8=FIX.4.4\x019=%d\x01", bodyLen)
	full := prefix + body
	var csum uint32
	for i := 0; i < len(full); i++ {
		csum += uint32(full[i])
	}
	return []byte(fmt.Sprintf("%s10=%03d\x01", full, csum%256))
}

func parseMsg(t *testing.T, data []byte) fix.Message {
	t.Helper()
	msg, err := fix.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return msg
}

func TestValidate_OK(t *testing.T) {
	d := loadTestDict(t)
	body := "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x0111=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"
	msg := parseMsg(t, buildFIXMsg(body))
	if err := d.Validate(&msg); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidate_Heartbeat_OK(t *testing.T) {
	d := loadTestDict(t)
	body := "35=0\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01"
	msg := parseMsg(t, buildFIXMsg(body))
	if err := d.Validate(&msg); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidate_UnknownMsgType(t *testing.T) {
	d := loadTestDict(t)
	body := "35=Z\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01"
	msg := parseMsg(t, buildFIXMsg(body))
	err := d.Validate(&msg)
	if err == nil {
		t.Fatal("expected error for unknown MsgType")
	}
	var ve *dictionary.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected dictionary.ValidationError, got %T", err)
	}
	if !errors.Is(err, dictionary.ErrUnknownMsgType) {
		t.Fatalf("expected dictionary.ErrUnknownMsgType, got: %v", err)
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	d := loadTestDict(t)
	// NewOrderSingle missing ClOrdID (tag 11)
	body := "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"
	msg := parseMsg(t, buildFIXMsg(body))
	err := d.Validate(&msg)
	if err == nil {
		t.Fatal("expected error for missing required tag")
	}
	var ve *dictionary.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected dictionary.ValidationError, got %T", err)
	}
	if !errors.Is(err, dictionary.ErrRequiredTag) {
		t.Fatalf("expected dictionary.ErrRequiredTag, got: %v", err)
	}
	if ve.Tag != 11 {
		t.Fatalf("expected tag 11, got %d", ve.Tag)
	}
}

func TestValidate_UnexpectedTag(t *testing.T) {
	d := loadTestDict(t)
	// Heartbeat with tag 999 which is not in dictionary at all
	body := "35=0\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01999=bad\x01"
	msg := parseMsg(t, buildFIXMsg(body))
	err := d.Validate(&msg)
	if err == nil {
		t.Fatal("expected error for unexpected tag")
	}
	var ve *dictionary.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected dictionary.ValidationError, got %T", err)
	}
	if !errors.Is(err, dictionary.ErrUnexpectedTag) {
		t.Fatalf("expected dictionary.ErrUnexpectedTag, got: %v", err)
	}
	if ve.Tag != 999 {
		t.Fatalf("expected tag 999, got %d", ve.Tag)
	}
}

func TestValidate_InvalidEnumValue(t *testing.T) {
	d := loadTestDict(t)
	// Side=9 is not a valid enum value (only 1 and 2 are)
	body := "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x0111=ORD1\x0154=9\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"
	msg := parseMsg(t, buildFIXMsg(body))
	err := d.Validate(&msg)
	if err == nil {
		t.Fatal("expected error for invalid enum value")
	}
	var ve *dictionary.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected dictionary.ValidationError, got %T", err)
	}
	if !errors.Is(err, dictionary.ErrInvalidEnumValue) {
		t.Fatalf("expected dictionary.ErrInvalidEnumValue, got: %v", err)
	}
	if ve.Tag != 54 {
		t.Fatalf("expected tag 54, got %d", ve.Tag)
	}
	if !errors.Is(err, fix.ErrFix2) {
		t.Fatalf("expected fix.ErrFix2, got: %v", err)
	}
	if want := `fix2: invalid value "9" for tag 54 (Side)`; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestParseAndValidate_Integration(t *testing.T) {
	d := loadTestDict(t)

	body := "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x0111=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=2\x0155=MSFT\x01"
	data := buildFIXMsg(body)

	msg, err := fix.ParseAndValidate(data, d)
	if err != nil {
		t.Fatalf("ParseAndValidate: %v", err)
	}
	if string(msg.MsgType()) != "D" {
		t.Fatalf("expected MsgType 'D', got %q", msg.MsgType())
	}
}

func TestParseAndValidate_ReaderIntegration(t *testing.T) {
	d := loadTestDict(t)

	body := "35=0\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01"
	data := buildFIXMsg(body)

	msg, err := fix.ParseFromReaderAndValidate(bytes.NewReader(data), d)
	if err != nil {
		t.Fatalf("ParseFromReaderAndValidate: %v", err)
	}
	if string(msg.MsgType()) != "0" {
		t.Fatalf("expected MsgType '0', got %q", msg.MsgType())
	}
}

func TestValidate_MultipleValueEnum(t *testing.T) {
	// FIX 4.2-4.4 MULTIPLEVALUESTRING and FIX 5.0 MULTIPLECHARVALUE /
	// MULTIPLESTRINGVALUE all hold space-separated values.
	for _, typ := range []string{"MULTIPLEVALUESTRING", "MULTIPLECHARVALUE", "MULTIPLESTRINGVALUE"} {
		t.Run(typ, func(t *testing.T) { testMultipleValueEnum(t, typ) })
	}
}

func testMultipleValueEnum(t *testing.T, typ string) {
	xml := `<fix type="FIX" major="5" minor="0" servicepack="2">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="NewOrderSingle" msgtype="D" msgcat="app">
      <field name="ExecInst" required="N"/>
      <field name="Side" required="N"/>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="18" name="ExecInst" type="` + typ + `">
      <value enum="1" description="NOT_HELD"/>
      <value enum="2" description="WORK"/>
      <value enum="G" description="ALL_OR_NONE"/>
    </field>
    <field number="54" name="Side" type="CHAR">
      <value enum="1" description="BUY"/>
      <value enum="2" description="SELL"/>
    </field>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tests := []struct {
		field string
		ok    bool
	}{
		{"18=1", true},
		{"18=1 2 G", true},
		{"18=1 3", false},  // 3 is not a value
		{"18=1  2", false}, // empty item
		{"18=1 2 ", false}, // trailing space
		{"18=", false},
		{"54=1", true},
		{"54=1 2", false}, // Side is single-valued
	}
	for _, tt := range tests {
		msg := parseMsg(t, buildFIXMsg("35=D\x01"+tt.field+"\x01"))
		err := d.Validate(&msg)
		if tt.ok && err != nil {
			t.Errorf("%q: unexpected error: %v", tt.field, err)
		}
		if !tt.ok && !errors.Is(err, dictionary.ErrInvalidEnumValue) {
			t.Errorf("%q: got %v, want ErrInvalidEnumValue", tt.field, err)
		}
	}
}

func TestValidate_OptionalComponentRequiredFields(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <field name="A" required="Y"/>
      <component name="Req" required="Y"/>
      <component name="Opt" required="N"/>
      <component name="OptWrap" required="N"/>
    </message>
  </messages>
  <components>
    <component name="Req"><field name="E" required="Y"/></component>
    <component name="Opt">
      <field name="B" required="Y"/>
      <field name="C" required="N"/>
      <component name="Inner" required="N"/>
    </component>
    <component name="Inner"><field name="D" required="Y"/><field name="H" required="N"/></component>
    <component name="OptWrap">
      <component name="ReqIn" required="Y"/>
      <field name="G" required="N"/>
    </component>
    <component name="ReqIn"><field name="F" required="Y"/></component>
  </components>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="101" name="A" type="STRING"/>
    <field number="102" name="B" type="STRING"/>
    <field number="103" name="C" type="STRING"/>
    <field number="104" name="D" type="STRING"/>
    <field number="105" name="E" type="STRING"/>
    <field number="106" name="F" type="STRING"/>
    <field number="107" name="G" type="STRING"/>
    <field number="108" name="H" type="STRING"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var required []int
	required = append(required, d.Messages["M"].RequiredTags()...)
	if got, want := slices.Sorted(slices.Values(required)), []int{101, 105}; !slices.Equal(got, want) {
		t.Errorf("unconditionally required = %v, want %v", got, want)
	}

	tests := []struct {
		name    string
		tags    []int
		missing int // 0 when valid
	}{
		{"optional components absent", []int{101, 105}, 0},
		{"Opt present through C, B missing", []int{101, 105, 103}, 102},
		{"Opt complete", []int{101, 105, 102, 103}, 0},
		{"Inner present, Opt complete", []int{101, 105, 102, 104}, 0},
		{"Inner present makes Opt present, B missing", []int{101, 105, 104}, 102},
		{"Inner present through H, D missing", []int{101, 105, 102, 108}, 104},
		{"required component field missing", []int{101}, 105},
		{"OptWrap present through G, F missing", []int{101, 105, 107}, 106},
		{"OptWrap complete", []int{101, 105, 107, 106}, 0},
	}
	for _, tt := range tests {
		var body strings.Builder
		body.WriteString("35=M\x01")
		for _, tag := range tt.tags {
			fmt.Fprintf(&body, "%d=x\x01", tag)
		}
		msg := parseMsg(t, buildFIXMsg(body.String()))
		err := d.Validate(&msg)
		if tt.missing == 0 {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tt.name, err)
			}
			continue
		}
		var ve *dictionary.ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, dictionary.ErrRequiredTag) || ve.Tag != tt.missing {
			t.Errorf("%s: got %v, want required tag %d missing", tt.name, err, tt.missing)
		}
	}
}

func TestValidate_OptionalComponentScopes(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header>
    <field name="MsgType" required="Y"/>
    <component name="Hop" required="N"/>
  </header>
  <trailer/>
  <messages>
    <message name="Grouped" msgtype="G" msgcat="app">
      <component name="WithGroup" required="N"/>
    </message>
    <message name="Shared" msgtype="S" msgcat="app">
      <component name="Opt" required="N"/>
      <group name="NoY" required="N"><field name="X" required="N"/></group>
    </message>
    <message name="Twice" msgtype="T" msgcat="app">
      <component name="Opt" required="N"/>
      <group name="NoZ" required="N"><component name="Opt" required="N"/></group>
    </message>
    <message name="UsesHopTag" msgtype="U" msgcat="app">
      <field name="HopOther" required="N"/>
    </message>
    <message name="SharesHeaderTag" msgtype="B" msgcat="app">
      <component name="BodyOpt" required="N"/>
    </message>
    <message name="Duplicated" msgtype="D" msgcat="app">
      <component name="Dup" required="N"/>
    </message>
  </messages>
  <components>
    <component name="WithGroup">
      <field name="F" required="Y"/>
      <group name="NoG" required="Y">
        <field name="X" required="N"/>
        <group name="NoH" required="N"><field name="W" required="N"/></group>
      </group>
    </component>
    <component name="BodyOpt">
      <field name="F2" required="Y"/>
      <field name="HopTime" required="N"/>
    </component>
    <component name="Dup">
      <field name="F3" required="Y"/>
      <field name="Y" required="N"/>
      <group name="NoK" required="N"><field name="Y" required="N"/></group>
    </component>
    <component name="Opt">
      <field name="F" required="Y"/>
      <field name="X" required="N"/>
    </component>
    <component name="Hop">
      <field name="HopID" required="Y"/>
      <field name="HopTime" required="N"/>
      <field name="HopOther" required="N"/>
      <field name="HopRef" required="N"/>
    </component>
  </components>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="101" name="F" type="STRING"/>
    <field number="102" name="X" type="STRING"/>
    <field number="103" name="NoG" type="NUMINGROUP"/>
    <field number="104" name="NoY" type="NUMINGROUP"/>
    <field number="105" name="NoZ" type="NUMINGROUP"/>
    <field number="106" name="W" type="STRING"/>
    <field number="107" name="NoH" type="NUMINGROUP"/>
    <field number="108" name="F2" type="STRING"/>
    <field number="109" name="F3" type="STRING"/>
    <field number="110" name="Y" type="STRING"/>
    <field number="111" name="NoK" type="NUMINGROUP"/>
    <field number="628" name="HopID" type="STRING"/>
    <field number="629" name="HopTime" type="STRING"/>
    <field number="630" name="HopOther" type="STRING"/>
    <field number="631" name="HopRef" type="STRING"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name    string
		body    string
		missing int // 0 when valid
	}{
		// Group contents show the enclosing optional component is present.
		{"group child alone activates component", "35=G\x01102=x\x01", 101},
		{"component with group complete", "35=G\x01101=f\x01103=1\x01102=x\x01", 0},
		{"component absent", "35=G\x01", 0},
		{"required group counter missing", "35=G\x01101=f\x01102=x\x01", 103},
		{"nested group child alone activates component", "35=G\x01106=w\x01", 101},
		// HopTime is also a header tag, so it cannot activate BodyOpt.
		{"body tag shared with header", "35=B\x01629=t\x01", 0},
		// Y occurs twice, both times inside Dup: it still activates Dup.
		{"tag repeated inside one component", "35=D\x01110=y\x01", 109},
		// X also occurs in group NoY, so it cannot activate Opt.
		{"shared tag from another group", "35=S\x01104=1\x01102=x\x01", 0},
		// Opt is referenced twice: its presence cannot be attributed, so it is
		// not enforced rather than rejecting valid messages.
		{"component referenced twice", "35=T\x01105=1\x01102=x\x01", 0},
		// Header conditions.
		{"header component absent", "35=G\x01", 0},
		{"header component partial", "35=G\x01631=r\x01", 628}, // HopRef is only in the header
		{"header component complete", "35=G\x01628=h\x01629=t\x01", 0},
		// HopOther is also a field of message U, so it cannot activate Hop.
		{"header tag also used in a body", "35=U\x01630=o\x01", 0},
	}
	for _, tt := range tests {
		msg := parseMsg(t, buildFIXMsg(tt.body))
		err := d.Validate(&msg)
		if tt.missing == 0 {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tt.name, err)
			}
			continue
		}
		var ve *dictionary.ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, dictionary.ErrRequiredTag) || ve.Tag != tt.missing {
			t.Errorf("%s: got %v, want required tag %d missing", tt.name, err, tt.missing)
		}
	}
}

func TestValidate_RequiredTagOnlyInGroup(t *testing.T) {
	// F is required directly and optional in NoG entries; Hops' HopID is
	// required in the header and optional in the NoHops header group; Opt's
	// required R must be direct even when Opt is present through its group.
	const xml = `<fix type="FIX" major="4" minor="4">
  <header>
    <field name="MsgType" required="Y"/>
    <field name="HopID" required="Y"/>
    <group name="NoHops" required="N"><field name="HopRef" required="N"/><field name="HopID" required="N"/></group>
  </header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <field name="F" required="Y"/>
      <group name="NoG" required="N"><field name="D" required="N"/><field name="F" required="N"/></group>
      <component name="Opt" required="N"/>
    </message>
  </messages>
  <components>
    <component name="Opt">
      <field name="R" required="Y"/>
      <group name="NoO" required="N"><field name="OD" required="N"/><field name="R" required="N"/></group>
    </component>
  </components>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="100" name="F" type="STRING"/>
    <field number="101" name="NoG" type="NUMINGROUP"/>
    <field number="102" name="D" type="STRING"/>
    <field number="110" name="HopID" type="STRING"/>
    <field number="111" name="NoHops" type="NUMINGROUP"/>
    <field number="112" name="HopRef" type="STRING"/>
    <field number="120" name="R" type="STRING"/>
    <field number="121" name="NoO" type="NUMINGROUP"/>
    <field number="122" name="OD" type="STRING"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tests := []struct {
		name    string
		body    string
		missing int // 0 when valid
	}{
		{"F only inside NoG", "35=M\x01110=h\x01101=1\x01102=d\x01100=inside\x01", 100},
		{"F direct after NoG", "35=M\x01110=h\x01101=1\x01102=d\x01100=inside\x01100=direct\x01", 0},
		{"F direct before NoG", "35=M\x01110=h\x01100=direct\x01101=1\x01102=d\x01100=inside\x01", 0},
		{"HopID only inside NoHops", "35=M\x01111=1\x01112=r\x01110=inside\x01100=f\x01", 110},
		{"HopID direct", "35=M\x01111=1\x01112=r\x01110=inside\x01110=direct\x01100=f\x01", 0},
		{"Opt present through NoO, R only inside", "35=M\x01110=h\x01100=f\x01121=1\x01122=o\x01120=inside\x01", 120},
		{"Opt present through NoO, R direct", "35=M\x01110=h\x01100=f\x01120=r\x01121=1\x01122=o\x01120=inside\x01", 0},
	}
	for _, tt := range tests {
		msg := parseMsg(t, buildFIXMsg(tt.body))
		err := d.Validate(&msg)
		if tt.missing == 0 {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tt.name, err)
			}
			continue
		}
		var ve *dictionary.ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, dictionary.ErrRequiredTag) || ve.Tag != tt.missing {
			t.Errorf("%s: got %v, want required tag %d missing", tt.name, err, tt.missing)
		}
	}
}

func TestValidate_RequiredTagGroupShapes(t *testing.T) {
	// F(100) and Sig(140) are required directly and optional in groups of
	// several shapes: a group starting with a nested group, one starting
	// with a DATA field (so entries start with its LENGTH), and a trailer
	// group.
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header>
  <trailer>
    <group name="NoT" required="N"><field name="TD" required="N"/><field name="Sig" required="N"/></group>
    <field name="Sig" required="Y"/>
  </trailer>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <field name="F" required="Y"/>
      <group name="NoP" required="N">
        <group name="NoC" required="N"><field name="CD" required="N"/><field name="F" required="N"/></group>
        <field name="PD" required="N"/>
      </group>
      <group name="NoD" required="N"><field name="EncodedText" required="N"/><field name="F" required="N"/></group>
      <group name="NoG" required="N"><field name="D" required="N"/><field name="F" required="N"/></group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="100" name="F" type="STRING"/>
    <field number="101" name="NoG" type="NUMINGROUP"/>
    <field number="102" name="D" type="STRING"/>
    <field number="140" name="Sig" type="STRING"/>
    <field number="141" name="NoT" type="NUMINGROUP"/>
    <field number="142" name="TD" type="STRING"/>
    <field number="150" name="NoP" type="NUMINGROUP"/>
    <field number="151" name="PD" type="STRING"/>
    <field number="152" name="NoC" type="NUMINGROUP"/>
    <field number="153" name="CD" type="STRING"/>
    <field number="160" name="NoD" type="NUMINGROUP"/>
    <field number="354" name="EncodedTextLen" type="LENGTH"/>
    <field number="355" name="EncodedText" type="DATA"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const sig = "140=s\x01"
	tests := []struct {
		name    string
		body    string
		missing int // 0 when valid
	}{
		{"F only in a group starting with a nested group", "35=M\x01150=1\x01152=1\x01153=c\x01100=in\x01151=p\x01" + sig, 100},
		{"F direct after a group starting with a nested group", "35=M\x01150=1\x01152=1\x01153=c\x01100=in\x01151=p\x01100=f\x01" + sig, 0},
		{"F only in a group starting with DATA", "35=M\x01160=1\x01354=2\x01355=ab\x01100=in\x01" + sig, 100},
		{"F direct after a group starting with DATA", "35=M\x01160=1\x01354=2\x01355=ab\x01100=in\x01100=f\x01" + sig, 0},
		{"Sig only in a trailer group", "35=M\x01100=f\x01141=1\x01142=t\x01140=in\x01", 140},
		{"Sig direct after a trailer group", "35=M\x01100=f\x01141=1\x01142=t\x01140=in\x01140=s\x01", 0},
		{"count above the entries, F only inside", "35=M\x01101=5\x01102=d\x01100=in\x01" + sig, 100},
		{"count above the entries, F repeated after", "35=M\x01101=5\x01102=d\x01100=in\x01100=f\x01" + sig, 0},
		{"entry without its delimiter is top level", "35=M\x01101=1\x01100=f\x01" + sig, 0},
	}
	for _, tt := range tests {
		msg := parseMsg(t, buildFIXMsg(tt.body))
		err := d.Validate(&msg)
		if tt.missing == 0 {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tt.name, err)
			}
			continue
		}
		var ve *dictionary.ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, dictionary.ErrRequiredTag) || ve.Tag != tt.missing {
			t.Errorf("%s: got %v, want required tag %d missing", tt.name, err, tt.missing)
		}
	}
}

func TestValidate_HeaderNoHops(t *testing.T) {
	d, err := xmldict.LoadFile("testdata/FIXT11.xml")
	if err != nil {
		t.Fatal(err)
	}
	const header = "35=0\x0149=S\x0156=T\x0134=1\x0152=20231001-12:00:00\x01"
	for name, body := range map[string]string{
		"no hops":  header,
		"two hops": header + "627=2\x01628=H1\x01629=20231001-12:00:00\x01628=H2\x01630=1\x01",
	} {
		msg := parseMsg(t, buildFIXMsg(body))
		if err := d.Validate(&msg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if spec := d.CounterSpec(d.Messages["0"], 627); spec == nil || spec.Delim != 628 {
		t.Fatalf("NoHops spec = %+v", spec)
	}
}

func TestValidate_DataLengthEdgeCases(t *testing.T) {
	// Z is optional with no number (tag 0); EncodedText(355) is declared but
	// its LENGTH, EncodedTextLen(354), is not; RawData(96) leads NoR entries
	// with its RawDataLength(95) declared later in the group.
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <field name="F" required="Y"/>
      <field name="Z" required="N"/>
      <group name="NoD" required="N"><field name="EncodedText" required="N"/><field name="F" required="N"/></group>
      <group name="NoR" required="N"><field name="RawData" required="N"/><field name="F" required="N"/><field name="RawDataLength" required="N"/></group>
    </message>
  </messages>
  <components/>
  <fields>
    <field name="Z" type="STRING"/>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="95" name="RawDataLength" type="LENGTH"/>
    <field number="96" name="RawData" type="DATA"/>
    <field number="100" name="F" type="STRING"/>
    <field number="160" name="NoD" type="NUMINGROUP"/>
    <field number="161" name="NoR" type="NUMINGROUP"/>
    <field number="355" name="EncodedText" type="DATA"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tests := []struct {
		name string
		body string
		err  error // nil when valid
		tag  int
	}{
		{"unknown tag with a tag 0 field declared", "35=M\x01100=f\x01999=x\x01", dictionary.ErrUnexpectedTag, 999},
		{"LENGTH of an allowed DATA field, undeclared", "35=M\x01100=f\x01160=1\x01354=2\x01355=ab\x01", nil, 0},
		{"F only in a group led by undeclared LENGTH", "35=M\x01160=1\x01354=2\x01355=ab\x01100=in\x01", dictionary.ErrRequiredTag, 100},
		{"F only in a group led by LENGTH declared later", "35=M\x01161=1\x0195=2\x0196=ab\x01100=in\x01", dictionary.ErrRequiredTag, 100},
		{"F direct after a group led by LENGTH declared later", "35=M\x01161=1\x0195=2\x0196=ab\x01100=in\x01100=f\x01", nil, 0},
	}
	for _, tt := range tests {
		msg := parseMsg(t, buildFIXMsg(tt.body))
		err := d.Validate(&msg)
		if tt.err == nil {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", tt.name, err)
			}
			continue
		}
		var ve *dictionary.ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, tt.err) || ve.Tag != tt.tag {
			t.Errorf("%s: got %v, want %v for tag %d", tt.name, err, tt.err, tt.tag)
		}
	}
}

func TestValidate_MalformedCounts(t *testing.T) {
	// Entries after a count of zero, a negative or non-numeric count, or
	// beyond the count are not group entries, as GroupsOf reads them: their
	// fields are top level, where F is the message's own but D, a field of
	// the group only, is not allowed. Nothing panics.
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <field name="F" required="Y"/>
      <group name="NoG" required="N"><field name="D" required="N"/><field name="F" required="N"/></group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="100" name="F" type="STRING"/>
    <field number="101" name="NoG" type="NUMINGROUP"/>
    <field number="102" name="D" type="STRING"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, count := range []string{"0", "-1", "x", "99999999999999999999"} {
		msg := parseMsg(t, buildFIXMsg("35=M\x01101="+count+"\x01102=d\x01100=f\x01"))
		if err := d.Validate(&msg); !isGroupFieldOutside(err, 102) {
			t.Errorf("count %s: %v, want D not allowed outside entries", count, err)
		}
	}
	// Undercounted: the second entry's F is top level.
	msg := parseMsg(t, buildFIXMsg("35=M\x01101=1\x01102=a\x01100=in\x01102=b\x01100=f\x01"))
	if err := d.Validate(&msg); !isGroupFieldOutside(err, 102) {
		t.Errorf("undercounted: %v, want D not allowed outside entries", err)
	}
}

// isGroupFieldOutside reports whether err rejects tag as not defined for the
// message type, as it is outside group entries.
func isGroupFieldOutside(err error, tag int) bool {
	var ve *dictionary.ValidationError
	return errors.Is(err, dictionary.ErrTagOutsideGroup) && errors.As(err, &ve) && ve.Tag == tag && ve.Reason == dictionary.ReasonTagNotDefinedForMessageType
}

func TestValidate_GroupWithLengthOnly(t *testing.T) {
	// NoL entries declare RawDataLength(95) but not RawData(96): builders
	// write both, so RawData does not end the entry and F inside stays in it.
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <field name="F" required="Y"/>
      <group name="NoL" required="N"><field name="D" required="N"/><field name="RawDataLength" required="N"/><field name="F" required="N"/></group>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="95" name="RawDataLength" type="LENGTH"/>
    <field number="96" name="RawData" type="DATA"/>
    <field number="100" name="F" type="STRING"/>
    <field number="101" name="NoL" type="NUMINGROUP"/>
    <field number="102" name="D" type="STRING"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	msg := parseMsg(t, buildFIXMsg("35=M\x01101=1\x01102=d\x0195=2\x0196=ab\x01100=in\x01"))
	var ve *dictionary.ValidationError
	if err := d.Validate(&msg); !errors.As(err, &ve) || !errors.Is(err, dictionary.ErrRequiredTag) || ve.Tag != 100 {
		t.Errorf("got %v, want required tag 100 missing", err)
	}
	// Two entries with their DATA field, and F direct: valid.
	msg = parseMsg(t, buildFIXMsg("35=M\x01100=f\x01101=2\x01102=d\x0195=2\x0196=ab\x01102=e\x0195=1\x0196=c\x01"))
	if err := d.Validate(&msg); err != nil {
		t.Errorf("valid message: %v", err)
	}
	// A pair the message declares neither field of stays unexpected.
	msg = parseMsg(t, buildFIXMsg("35=M\x01100=f\x0193=1\x0189=x\x01"))
	if err := d.Validate(&msg); !errors.Is(err, dictionary.ErrUnexpectedTag) {
		t.Errorf("Signature pair: got %v, want unexpected tag", err)
	}
}

func TestValidate_ErrorMessages(t *testing.T) {
	d := loadTestDict(t)
	const header = "49=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01"
	const order = "11=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"
	for _, tc := range []struct {
		body, want string
		tag        int
		reason     dictionary.RejectReason
	}{
		{"35=D\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01" + order, "fix2: required header tag 49 missing", 49, dictionary.ReasonRequiredTagMissing},
		{"35=D\x01" + header + "54=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01", "fix2: required tag 11 missing for message NewOrderSingle", 11, dictionary.ReasonRequiredTagMissing},
		{"35=D\x01" + header + order + "999=x\x01", "fix2: tag 999 not defined by the dictionary, in message NewOrderSingle", 999, dictionary.ReasonInvalidTagNumber},
		{"35=D\x01" + header + order + "112=x\x01", "fix2: tag 112 (TestReqID) not allowed in message NewOrderSingle", 112, dictionary.ReasonTagNotDefinedForMessageType},
		{"35=D\x01" + header + "11=ORD1\x0154=9\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01", `fix2: invalid value "9" for tag 54 (Side)`, 54, dictionary.ReasonValueIsIncorrect},
		{"35=Z\x01" + header, `fix2: unknown MsgType "Z"`, 35, dictionary.ReasonInvalidMsgType},
	} {
		msg := parseMsg(t, buildFIXMsg(tc.body))
		err := d.Validate(&msg)
		var ve *dictionary.ValidationError
		if !errors.As(err, &ve) || err.Error() != tc.want || ve.Tag != tc.tag || ve.Reason != tc.reason {
			t.Errorf("got %v (%+v), want %q, tag %d, reason %d", err, ve, tc.want, tc.tag, tc.reason)
		}
	}
}

func TestRejectReason_String(t *testing.T) {
	if got := dictionary.ReasonRequiredTagMissing.String(); got != "Required tag missing" {
		t.Errorf("String = %q", got)
	}
	if got := dictionary.RejectReason(1234).String(); got != "Other" {
		t.Errorf("unknown reason String = %q", got)
	}
}

func TestValidate_DoesNotAllocate(t *testing.T) {
	d := loadTestDict(t)
	const header = "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01"
	const order = "11=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"
	for _, body := range []string{header + order, header + order + "78=2\x0179=A\x0179=B\x01"} {
		msg := parseMsg(t, buildFIXMsg(body))
		if allocs := testing.AllocsPerRun(100, func() { _ = d.Validate(&msg) }); allocs != 0 {
			t.Errorf("Validate allocated %v times for %q", allocs, body)
		}
	}
}

// slotsDict returns a dictionary whose message M requires n fields, tags 50000
// to 50000+n-1, after a header requiring MsgType: n+1 presence slots.
func slotsDict(t *testing.T, n int) *dictionary.Dict {
	t.Helper()
	var refs, defs strings.Builder
	for i := range n {
		fmt.Fprintf(&refs, `<field name="F%d" required="Y"/>`, i)
		fmt.Fprintf(&defs, `<field number="%d" name="F%d" type="STRING"/>`, 50000+i, i)
	}
	d, err := xmldict.Load(strings.NewReader(`<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header><trailer/>
  <messages><message name="M" msgtype="M" msgcat="app">` + refs.String() + `</message></messages>
  <components/><fields><field number="35" name="MsgType" type="STRING"/>` + defs.String() + `</fields></fix>`))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestValidate_SlotBoundaries(t *testing.T) {
	for _, n := range []int{255, 256, 300} { // 256, 257 and 301 slots
		d := slotsDict(t, n)
		var body strings.Builder
		body.WriteString("35=M\x01")
		for i := range n {
			fmt.Fprintf(&body, "%d=x\x01", 50000+i)
		}
		msg := parseMsg(t, buildFIXMsg(body.String()))
		if err := d.Validate(&msg); err != nil {
			t.Errorf("%d slots, all present: %v", n+1, err)
		}
		allocs := testing.AllocsPerRun(10, func() { _ = d.Validate(&msg) })
		if n+1 <= 256 && allocs != 0 {
			t.Errorf("%d slots: Validate allocated %v times", n+1, allocs)
		}
		// The last slot missing.
		short := strings.Replace(body.String(), fmt.Sprintf("%d=x\x01", 50000+n-1), "", 1)
		msg = parseMsg(t, buildFIXMsg(short))
		var ve *dictionary.ValidationError
		if err := d.Validate(&msg); !errors.As(err, &ve) || ve.Tag != 50000+n-1 {
			t.Errorf("%d slots, last missing: got %v", n+1, err)
		}
	}
}

func TestValidate_CompiledEdgeCases(t *testing.T) {
	// BeginString declared in the body with enums is still never checked;
	// SenderCompID is required in the header and in the body (one slot);
	// Code has a long and a single-byte value.
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/><field name="SenderCompID" required="Y"/></header><trailer/>
  <messages><message name="M" msgtype="M" msgcat="app">
    <field name="BeginString" required="N"/><field name="SenderCompID" required="Y"/><field name="Code" required="N"/>
  </message></messages>
  <components/>
  <fields>
    <field number="8" name="BeginString" type="STRING"><value enum="X" description="X"/></field>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="100" name="Code" type="STRING"><value enum="LONG" description="L"/><value enum="S" description="S"/></field>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct {
		body string
		err  error
		tag  int
	}{
		{"35=M\x0149=s\x01", nil, 0},
		{"35=M\x01", dictionary.ErrRequiredTag, 49}, // header first
		{"35=M\x0149=s\x01100=LONG\x01", nil, 0},
		{"35=M\x0149=s\x01100=S\x01", nil, 0},
		{"35=M\x0149=s\x01100=LONGER\x01", dictionary.ErrInvalidEnumValue, 100},
		{"35=M\x0149=s\x01100=L\x01", dictionary.ErrInvalidEnumValue, 100},
		// Missing required tag, unexpected tag and invalid enum: the
		// required tag is reported.
		{"35=M\x01999=x\x01100=Q\x01", dictionary.ErrRequiredTag, 49},
		// Unexpected tag before invalid enum: the first in field order.
		{"35=M\x0149=s\x01999=x\x01100=Q\x01", dictionary.ErrUnexpectedTag, 999},
		{"35=M\x0149=s\x01100=Q\x01999=x\x01", dictionary.ErrInvalidEnumValue, 100},
	} {
		msg := parseMsg(t, buildFIXMsg(tc.body))
		err := d.Validate(&msg)
		var ve *dictionary.ValidationError
		if tc.err == nil && err != nil || tc.err != nil && (!errors.Is(err, tc.err) || !errors.As(err, &ve) || ve.Tag != tc.tag) {
			t.Errorf("%q: got %v, want %v for tag %d", tc.body, err, tc.err, tc.tag)
		}
	}
	// Changing the loaded dictionary does not change validation.
	d.Fields[100].Enums["LONGER"] = "added"
	msg := parseMsg(t, buildFIXMsg("35=M\x0149=s\x01100=LONGER\x01"))
	if err := d.Validate(&msg); !errors.Is(err, dictionary.ErrInvalidEnumValue) {
		t.Errorf("after editing the enums: got %v, want the compiled result", err)
	}
}

func TestValidate_Concurrent(t *testing.T) {
	d := loadTestDict(t)
	const header = "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01"
	const order = "11=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"
	good := parseMsg(t, buildFIXMsg(header+order+"78=2\x0179=A\x0179=B\x01"))
	bad := parseMsg(t, buildFIXMsg(header+"54=9\x01"))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				if err := d.Validate(&good); err != nil {
					t.Error(err)
					return
				}
				if err := d.Validate(&bad); err == nil {
					t.Error("bad message validated")
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestValidate_ManyEnvelopeGroupsDoNotAllocate(t *testing.T) {
	// Five header groups: their specs are found by counter, and
	// a message without groups of its own uses them as they are.
	var header, fields strings.Builder
	fields.WriteString(`<field number="35" name="MsgType" type="STRING"/><field number="100" name="F" type="STRING"/>`)
	for i := range 5 {
		fmt.Fprintf(&header, `<group name="NoH%d"><field name="F"/></group>`, i)
		fmt.Fprintf(&fields, `<field number="%d" name="NoH%d" type="NUMINGROUP"/>`, 1000+2*i, i)
	}
	d, err := xmldict.Load(strings.NewReader(`<fix major="4" minor="4"><header><field name="MsgType" required="Y"/>` + header.String() +
		`</header><trailer/><messages><message name="M" msgtype="M"><field name="F"/></message></messages><components/><fields>` + fields.String() + `</fields></fix>`))
	if err != nil {
		t.Fatal(err)
	}
	msg := parseMsg(t, buildFIXMsg("35=M\x011000=1\x01100=g\x01100=f\x01"))
	if err := d.Validate(&msg); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = d.Validate(&msg) }); allocs != 0 {
		t.Errorf("Validate allocated %v times", allocs)
	}
}

func TestNew_Idempotent(t *testing.T) {
	d := loadTestDict(t)
	nos := d.Messages["D"]
	enums, required := d.EnumChecks(), len(nos.RequiredTags())
	dictionary.New(d)
	if d.EnumChecks() != enums || len(nos.RequiredTags()) != required {
		t.Errorf("after a second dictionary.New: %d enum checks, %d required tags; want %d and %d", d.EnumChecks(), len(nos.RequiredTags()), enums, required)
	}
	msg := parseMsg(t, buildFIXMsg("35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x0111=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"))
	if err := d.Validate(&msg); err != nil {
		t.Errorf("after a second dictionary.New: %v", err)
	}
}

func TestNew_IdempotentWithConditionsAndEnvelopeGroups(t *testing.T) {
	// Five header groups and an optional
	// component in the body requiring B when C is present.
	var header, fields strings.Builder
	fields.WriteString(`<field number="35" name="MsgType" type="STRING"/><field number="100" name="F" type="STRING"/>` +
		`<field number="102" name="B" type="STRING"/><field number="103" name="C" type="STRING"/>`)
	for i := range 5 {
		fmt.Fprintf(&header, `<group name="NoH%d"><field name="F"/></group>`, i)
		fmt.Fprintf(&fields, `<field number="%d" name="NoH%d" type="NUMINGROUP"/>`, 1000+2*i, i)
	}
	d, err := xmldict.Load(strings.NewReader(`<fix major="4" minor="4"><header><field name="MsgType" required="Y"/>` + header.String() +
		`</header><trailer/><messages><message name="M" msgtype="M"><field name="F" required="Y"/><component name="Opt" required="N"/></message></messages>` +
		`<components><component name="Opt"><field name="B" required="Y"/><field name="C" required="N"/></component></components><fields>` + fields.String() + `</fields></fix>`))
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		for body, want := range map[string]error{
			"35=M\x011000=1\x01100=g\x01100=f\x01": nil,
			"35=M\x011000=1\x01100=g\x01":          dictionary.ErrRequiredTag, // F only in a header group
			"35=M\x01100=f\x01103=c\x01":           dictionary.ErrRequiredTag, // Opt present, B missing
			"35=M\x01100=f\x01103=c\x01102=b\x01":  nil,
			"35=M\x01100=f\x01999=x\x01":           dictionary.ErrUnexpectedTag,
		} {
			msg := parseMsg(t, buildFIXMsg(body))
			if err := d.Validate(&msg); want == nil && err != nil || want != nil && !errors.Is(err, want) {
				t.Errorf("%s: %q: got %v, want %v", when, body, err, want)
			}
		}
	}
	check("after Load")
	m := d.Messages["M"]
	conds := m.CompiledConds()
	dictionary.New(d)
	if m.CompiledConds() != conds {
		t.Errorf("after a second dictionary.New: %d conditions, want %d", m.CompiledConds(), conds)
	}
	for i := range 5 {
		if d.CounterSpec(m, 1000+2*i) == nil {
			t.Errorf("after a second dictionary.New: no spec for NoH%d", i)
		}
	}
	check("after a second New")
	// Down to four header groups: the fifth's counter no longer has a spec.
	d.HeaderGroups = d.HeaderGroups[:4]
	dictionary.New(d)
	if d.CounterSpec(m, 1008) != nil || d.CounterSpec(m, 1006) == nil {
		t.Error("with four header groups: wrong counter specs")
	}
	check("with fewer header groups")
}

func TestCompiledBodyTable(t *testing.T) {
	// The compiled table of NewOrderSingle holds Symbol (55), from the
	// Instrument component, the NoAllocs counter (78) and its child (79).
	nos := loadTestDict(t).Messages["D"]
	for _, tag := range []int{55, 78, 79} {
		if !nos.Allows(tag) {
			t.Errorf("compiled table lacks tag %d", tag)
		}
	}
	if nos.Allows(999) {
		t.Error("compiled table holds tag 999")
	}
}

func TestValidate_EnvelopeActivationPerMessage(t *testing.T) {
	// The header's optional component H requires A(101) and has B(102). X
	// declares B in its body, Y does not: only X's B can be its own.
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/><component name="H" required="N"/></header>
  <trailer><component name="T" required="N"/></trailer>
  <messages>
    <message name="X" msgtype="X" msgcat="app"><field name="B" required="N"/><field name="D" required="N"/></message>
    <message name="Y" msgtype="Y" msgcat="app"/>
  </messages>
  <components>
    <component name="H"><field name="A" required="Y"/><field name="B" required="N"/></component>
    <component name="T"><field name="C" required="Y"/><field name="D" required="N"/></component>
  </components>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="101" name="A" type="STRING"/>
    <field number="102" name="B" type="STRING"/>
    <field number="103" name="C" type="STRING"/>
    <field number="104" name="D" type="STRING"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body    string
		missing int // 0 when valid
	}{
		{"35=Y\x01102=b\x01", 101},        // B can only be H's
		{"35=Y\x01102=b\x01101=a\x01", 0}, // H complete
		{"35=X\x01102=b\x01", 0},          // B may be X's own
		{"35=Y\x01104=d\x01", 103},        // the trailer's T, the same way
		{"35=X\x01104=d\x01", 0},          // D may be X's own
		{"35=Y\x01", 0},                   // no component present
	} {
		msg := parseMsg(t, buildFIXMsg(tc.body))
		err := d.Validate(&msg)
		var ve *dictionary.ValidationError
		if tc.missing == 0 && err != nil || tc.missing != 0 && (!errors.As(err, &ve) || ve.Tag != tc.missing) {
			t.Errorf("%q: got %v, want missing %d", tc.body, err, tc.missing)
		}
	}
}

func TestValidate_ManyGroupsIsLinear(t *testing.T) {
	// The same message, 8,000 repetitions of an allowed field before a
	// group counter, validated against a message with 500 optional groups
	// or 16,000: each field's counter lookup is one table lookup, not a
	// scan of the group specs.
	load := func(n int) *dictionary.Dict {
		var groups, fields strings.Builder
		fields.WriteString(`<field number="35" name="MsgType" type="STRING"/><field number="100" name="F" type="STRING"/>`)
		for i := range n {
			fmt.Fprintf(&groups, `<group name="NoG%d"><field name="F"/></group>`, i)
			fmt.Fprintf(&fields, `<field number="%d" name="NoG%d" type="NUMINGROUP"/>`, 1000+i, i)
		}
		d, err := xmldict.Load(strings.NewReader(`<fix major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/>` +
			`<messages><message name="M" msgtype="M"><field name="F"/>` + groups.String() + `</message></messages><components/><fields>` + fields.String() + `</fields></fix>`))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	msg := parseMsg(t, buildFIXMsg("35=M\x01"+strings.Repeat("100=f\x01", 8000)+"1000=0\x01"))
	best := func(groups int) time.Duration {
		d := load(groups)
		best := time.Duration(math.MaxInt64)
		for range 5 {
			start := time.Now()
			if err := d.Validate(&msg); err != nil {
				t.Fatal(err)
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	if r := float64(best(16000)) / float64(best(500)); r > 8 {
		t.Errorf("32x the groups took %.1fx longer for the same message", r)
	}
}
