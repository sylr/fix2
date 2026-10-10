package dictionary

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
)

// ---------------------------------------------------------------------------
// Embedded minimal FIX 4.4 dictionary
// ---------------------------------------------------------------------------

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

func loadTestDict(t *testing.T) *Dict {
	t.Helper()
	d, err := Load(strings.NewReader(testDictXML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return d
}

// ---------------------------------------------------------------------------
// Load tests
// ---------------------------------------------------------------------------

func TestLoad_Basic(t *testing.T) {
	d := loadTestDict(t)

	if d.Type != "FIX" || d.Major != "4" || d.Minor != "4" {
		t.Fatalf("dict version: got %s %s.%s", d.Type, d.Major, d.Minor)
	}
	if len(d.Fields) != 16 {
		t.Fatalf("expected 16 fields, got %d", len(d.Fields))
	}
	if len(d.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(d.Messages))
	}
	if _, ok := d.Messages["0"]; !ok {
		t.Fatal("missing Heartbeat message")
	}
	if _, ok := d.Messages["D"]; !ok {
		t.Fatal("missing NewOrderSingle message")
	}
}

func TestLoad_InvalidXML(t *testing.T) {
	_, err := Load(strings.NewReader("<broken"))
	if err == nil {
		t.Fatal("expected error for invalid XML")
	}
}

func TestLoad_UnknownComponent(t *testing.T) {
	xml := `<?xml version="1.0"?>
<fix type="FIX" major="4" minor="4">
  <header/>
  <trailer/>
  <messages>
    <message name="Test" msgtype="Z" msgcat="app">
      <component name="DoesNotExist" required="Y"/>
    </message>
  </messages>
  <components/>
  <fields/>
</fix>`
	_, err := Load(strings.NewReader(xml))
	if err == nil {
		t.Fatal("expected error for unknown component")
	}
	if !strings.Contains(err.Error(), "DoesNotExist") {
		t.Fatalf("error should mention component name, got: %v", err)
	}
}

func TestLoad_UnknownField(t *testing.T) {
	xml := `<?xml version="1.0"?>
<fix type="FIX" major="4" minor="4">
  <header>
    <field name="NoSuchField" required="Y"/>
  </header>
  <trailer/>
  <messages/>
  <components/>
  <fields/>
</fix>`
	_, err := Load(strings.NewReader(xml))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "NoSuchField") {
		t.Fatalf("error should mention field name, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Resolution tests
// ---------------------------------------------------------------------------

func TestResolve_ComponentFlattened(t *testing.T) {
	d := loadTestDict(t)
	nos := d.Messages["D"]

	// Symbol (tag 55) should be in the flattened field list via Instrument component.
	if !nos.fieldSet[55] {
		t.Fatal("Symbol (55) should be in NewOrderSingle fields via Instrument component")
	}
}

func TestResolve_GroupCounterAndChildren(t *testing.T) {
	d := loadTestDict(t)
	nos := d.Messages["D"]

	// NoAllocs counter (tag 78) should be present.
	if !nos.fieldSet[78] {
		t.Fatal("NoAllocs counter (78) should be in NewOrderSingle fields")
	}
	// AllocAccount (tag 79) should be present (group child).
	if !nos.fieldSet[79] {
		t.Fatal("AllocAccount (79) should be in NewOrderSingle fields as group child")
	}
}

func TestResolve_GroupStructure(t *testing.T) {
	d := loadTestDict(t)
	nos := d.Messages["D"]

	if len(nos.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(nos.Groups))
	}

	g := nos.Groups[0]
	if g.Name != "NoAllocs" {
		t.Errorf("group name = %q, want %q", g.Name, "NoAllocs")
	}
	if g.CounterTag != 78 {
		t.Errorf("CounterTag = %d, want 78", g.CounterTag)
	}
	if g.DelimTag != 79 {
		t.Errorf("DelimTag = %d, want 79", g.DelimTag)
	}
	if g.Required {
		t.Error("group should not be required")
	}
	if len(g.Fields) != 1 {
		t.Fatalf("expected 1 child field, got %d", len(g.Fields))
	}
	if g.Fields[0].Tag != 79 {
		t.Errorf("child field tag = %d, want 79", g.Fields[0].Tag)
	}
}

// nestedDictXML exercises the shapes FIX 5.0 SP2 relies on: a group whose
// first child is a component, a group reached through nested components, and
// a group nested inside another group through a component.
const nestedDictXML = `<fix type="FIX" major="5" minor="0" servicepack="2">
  <header/>
  <trailer/>
  <messages>
    <message name="NewOrderMultileg" msgtype="AB" msgcat="app">
      <field name="ClOrdID" required="Y"/>
      <component name="Instrument" required="N"/>
      <group name="NoLegs" required="N">
        <component name="InstrumentLeg" required="N"/>
        <field name="LegQty" required="N"/>
        <component name="NestedParties" required="N"/>
      </group>
    </message>
  </messages>
  <components>
    <component name="Instrument">
      <field name="Symbol" required="N"/>
      <component name="SecAltIDGrp" required="N"/>
    </component>
    <component name="SecAltIDGrp">
      <group name="NoSecurityAltID" required="N">
        <field name="SecurityAltID" required="N"/>
        <field name="SecurityAltIDSource" required="N"/>
      </group>
    </component>
    <component name="InstrumentLeg">
      <field name="LegSymbol" required="N"/>
      <field name="LegSide" required="N"/>
    </component>
    <component name="NestedParties">
      <group name="NoNestedPartyIDs" required="N">
        <field name="NestedPartyID" required="N"/>
        <field name="NestedPartyRole" required="N"/>
      </group>
    </component>
  </components>
  <fields>
    <field number="11" name="ClOrdID" type="STRING"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="454" name="NoSecurityAltID" type="NUMINGROUP"/>
    <field number="455" name="SecurityAltID" type="STRING"/>
    <field number="456" name="SecurityAltIDSource" type="STRING"/>
    <field number="555" name="NoLegs" type="NUMINGROUP"/>
    <field number="600" name="LegSymbol" type="STRING"/>
    <field number="624" name="LegSide" type="CHAR"/>
    <field number="687" name="LegQty" type="QTY"/>
    <field number="539" name="NoNestedPartyIDs" type="NUMINGROUP"/>
    <field number="524" name="NestedPartyID" type="STRING"/>
    <field number="538" name="NestedPartyRole" type="INT"/>
  </fields>
</fix>`

func TestResolve_NestedGroupsAndChildOrder(t *testing.T) {
	d, err := Load(strings.NewReader(nestedDictXML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	md := d.Messages["AB"]

	tags := func(refs []FieldRef) []int {
		var out []int
		for _, r := range refs {
			out = append(out, r.Tag)
		}
		return out
	}

	// Every tag in XML order, components expanded in place.
	if got, want := tags(md.Fields), []int{11, 55, 454, 455, 456, 555, 600, 624, 687, 539, 524, 538}; !slices.Equal(got, want) {
		t.Errorf("message Fields = %v, want %v", got, want)
	}

	// Top-level groups in XML order, including one two components deep.
	if len(md.Groups) != 2 {
		t.Fatalf("expected 2 top-level groups, got %d", len(md.Groups))
	}
	alt, legs := md.Groups[0], md.Groups[1]
	if alt.Name != "NoSecurityAltID" || alt.DelimTag != 455 {
		t.Errorf("group 0 = %s delim %d, want NoSecurityAltID delim 455", alt.Name, alt.DelimTag)
	}

	// NoLegs starts with the InstrumentLeg component: its first field is the delimiter.
	if legs.Name != "NoLegs" || legs.DelimTag != 600 {
		t.Errorf("group 1 = %s delim %d, want NoLegs delim 600", legs.Name, legs.DelimTag)
	}
	if got, want := tags(legs.Fields), []int{600, 624, 687}; !slices.Equal(got, want) {
		t.Errorf("NoLegs Fields = %v, want %v", got, want)
	}

	// NoNestedPartyIDs is nested in NoLegs through the NestedParties component.
	if len(legs.Groups) != 1 {
		t.Fatalf("expected 1 group nested in NoLegs, got %d", len(legs.Groups))
	}
	if nested := legs.Groups[0]; nested.Name != "NoNestedPartyIDs" || nested.CounterTag != 539 || nested.DelimTag != 524 {
		t.Errorf("nested group = %s counter %d delim %d, want NoNestedPartyIDs counter 539 delim 524", nested.Name, nested.CounterTag, nested.DelimTag)
	}
}

func TestLoad_ComponentCycle(t *testing.T) {
	xml := `<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app"><component name="A" required="N"/></message>
  </messages>
  <components>
    <component name="A"><component name="B" required="N"/></component>
    <component name="B"><component name="A" required="N"/></component>
  </components>
  <fields/>
</fix>`
	_, err := Load(strings.NewReader(xml))
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validation tests
// ---------------------------------------------------------------------------

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

func parseMsg(t *testing.T, data []byte) fix2.Message {
	t.Helper()
	msg, err := fix2.Parse(data)
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
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T", err)
	}
	if !errors.Is(err, ErrUnknownMsgType) {
		t.Fatalf("expected ErrUnknownMsgType, got: %v", err)
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
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T", err)
	}
	if !errors.Is(err, ErrRequiredTag) {
		t.Fatalf("expected ErrRequiredTag, got: %v", err)
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
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T", err)
	}
	if !errors.Is(err, ErrUnexpectedTag) {
		t.Fatalf("expected ErrUnexpectedTag, got: %v", err)
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
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T", err)
	}
	if !errors.Is(err, ErrInvalidEnumValue) {
		t.Fatalf("expected ErrInvalidEnumValue, got: %v", err)
	}
	if ve.Tag != 54 {
		t.Fatalf("expected tag 54, got %d", ve.Tag)
	}
	if !errors.Is(err, fix2.ErrFix2) {
		t.Fatalf("expected fix2.ErrFix2, got: %v", err)
	}
	if want := `fix2: invalid value "9" for tag 54 (Side)`; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}

// ---------------------------------------------------------------------------
// Integration: fix2.ParseAndValidate end-to-end
// ---------------------------------------------------------------------------

func TestParseAndValidate_Integration(t *testing.T) {
	d := loadTestDict(t)

	body := "35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x0111=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=2\x0155=MSFT\x01"
	data := buildFIXMsg(body)

	msg, err := fix2.ParseAndValidate(data, d)
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

	msg, err := fix2.ParseFromReaderAndValidate(bytes.NewReader(data), d)
	if err != nil {
		t.Fatalf("ParseFromReaderAndValidate: %v", err)
	}
	if string(msg.MsgType()) != "0" {
		t.Fatalf("expected MsgType '0', got %q", msg.MsgType())
	}
}

func TestValidate_MultipleValueEnum(t *testing.T) {
	const xml = `<fix type="FIX" major="5" minor="0" servicepack="2">
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
    <field number="18" name="ExecInst" type="MULTIPLECHARVALUE">
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
	d, err := Load(strings.NewReader(xml))
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
		if !tt.ok && !errors.Is(err, ErrInvalidEnumValue) {
			t.Errorf("%q: got %v, want ErrInvalidEnumValue", tt.field, err)
		}
	}
}

func TestLoad_ExponentialExpansion(t *testing.T) {
	for name, c0 := range map[string]string{
		"leaf field": `<component name="C0"><field name="F" required="N"/></component>`,
		"empty leaf": `<component name="C0"/>`,
	} {
		t.Run(name, func(t *testing.T) { testExponentialExpansion(t, c0) })
	}
}

func testExponentialExpansion(t *testing.T, c0 string) {
	// Each Cn references C(n-1) twice, so M expands to 2^30 copies of C0
	// although the dictionary is tiny and acyclic.
	const depth = 30
	var comps strings.Builder
	comps.WriteString(c0)
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&comps, `<component name="C%d"><component name="C%d" required="N"/><component name="C%d" required="N"/></component>`, i, i-1, i-1)
	}
	xml := fmt.Sprintf(`<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages><message name="M" msgtype="M" msgcat="app"><component name="C%d" required="N"/></message></messages>
  <components>%s</components>
  <fields><field number="1" name="F" type="STRING"/></fields>
</fix>`, depth, comps.String())

	start := time.Now()
	_, err := Load(strings.NewReader(xml))
	if err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
		t.Fatalf("expected expansion error, got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Load took %v, want it to stop early", d)
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
	d, err := Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := slices.Sorted(maps.Keys(d.Messages["M"].required)), []int{101, 105}; !slices.Equal(got, want) {
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
		var ve *ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, ErrRequiredTag) || ve.Tag != tt.missing {
			t.Errorf("%s: got %v, want required tag %d missing", tt.name, err, tt.missing)
		}
	}
}
