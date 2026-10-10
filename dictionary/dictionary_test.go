package dictionary

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

	if err := loadWithin(t, xml); err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
		t.Fatalf("expected expansion error, got %v", err)
	}
}

// loadWithin loads a dictionary and fails the test if Load does not return
// within 10 seconds, so a resolver that hangs fails instead of timing out
// the whole test binary.
func loadWithin(t *testing.T, xml string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := Load(strings.NewReader(xml))
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Load did not return within 10s")
		return nil
	}
}

func TestLoad_DeepNesting(t *testing.T) {
	// A linear chain of components: each Ci references C(i-1) once, ending in
	// a field. It needs few visits but nests far beyond maxDepth.
	chain := func(n int, leafField string) string {
		var comps strings.Builder
		fmt.Fprintf(&comps, `<component name="C0"><field name="%s" required="N"/></component>`, leafField)
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&comps, `<component name="C%d"><component name="C%d" required="N"/></component>`, i, i-1)
		}
		return fmt.Sprintf(`<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages><message name="M" msgtype="M" msgcat="app"><component name="C%d" required="N"/></message></messages>
  <components>%s</components>
  <fields><field number="1" name="F" type="STRING"/></fields>
</fix>`, n, comps.String())
	}
	for name, xml := range map[string]string{
		"known leaf":   chain(10_000, "F"),
		"unknown leaf": chain(10_000, "Missing"), // error wrapping must not grow with depth
		"at the limit": chain(maxDepth-1, "F"),   // C(n) ... C0 is n+1 component levels
		"one past":     chain(maxDepth, "F"),
	} {
		t.Run(name, func(t *testing.T) {
			err := loadWithin(t, xml)
			if name == "at the limit" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "nested deeper than") {
				t.Fatalf("expected depth error, got %v", err)
			}
			if len(err.Error()) > 10_000 {
				t.Errorf("error message is %d bytes long", len(err.Error()))
			}
		})
	}
}

func TestLoad_NestedGroupCopiesCharged(t *testing.T) {
	// C0 holds 40,000 fields and each Ci wraps C(i-1) in a group. Few items
	// are visited, but every group level copies all its descendants into its
	// parent: about 30 x 40,000 copied refs, above maxVisits.
	const fields, levels = 40_000, 30
	var defs, c0, comps strings.Builder
	c0.WriteString(`<component name="C0">`)
	for i := range fields {
		fmt.Fprintf(&defs, `<field number="%d" name="F%d" type="STRING"/>`, 1000+i, i)
		fmt.Fprintf(&c0, `<field name="F%d" required="N"/>`, i)
	}
	c0.WriteString(`</component>`)
	for i := 1; i <= levels; i++ {
		fmt.Fprintf(&defs, `<field number="%d" name="NoG%d" type="NUMINGROUP"/>`, 100+i, i)
		fmt.Fprintf(&comps, `<component name="C%d"><group name="NoG%d" required="N"><component name="C%d" required="N"/></group></component>`, i, i, i-1)
	}
	xml := fmt.Sprintf(`<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages><message name="M" msgtype="M" msgcat="app"><component name="C%d" required="N"/></message></messages>
  <components>%s%s</components>
  <fields>%s</fields>
</fix>`, levels, c0.String(), comps.String(), defs.String())

	if err := loadWithin(t, xml); err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
		t.Fatalf("expected expansion error, got %v", err)
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
	d, err := Load(strings.NewReader(xml))
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
		var ve *ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, ErrRequiredTag) || ve.Tag != tt.missing {
			t.Errorf("%s: got %v, want required tag %d missing", tt.name, err, tt.missing)
		}
	}
}

// endless yields 'a' forever.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestLoad_TooLarge(t *testing.T) {
	// An XML comment that never ends: Load must stop after maxDictSize bytes.
	r := io.MultiReader(strings.NewReader("<fix><!--"), endless{})
	_, err := Load(r)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("expected size error, got %v", err)
	}
}

func TestLoad_ConditionTrackingCharged(t *testing.T) {
	// 60 nested optional components around 20,000 fields: few items are
	// visited, but each field is recorded in all 60 enclosing conditions.
	const fields, levels = 20_000, 60
	var defs, c0, comps strings.Builder
	c0.WriteString(`<component name="C0"><field name="R" required="Y"/>`)
	defs.WriteString(`<field number="999" name="R" type="STRING"/>`)
	for i := range fields {
		fmt.Fprintf(&defs, `<field number="%d" name="F%d" type="STRING"/>`, 1000+i, i)
		fmt.Fprintf(&c0, `<field name="F%d" required="N"/>`, i)
	}
	c0.WriteString(`</component>`)
	for i := 1; i <= levels; i++ {
		fmt.Fprintf(&comps, `<component name="C%d"><component name="C%d" required="N"/></component>`, i, i-1)
	}
	xml := fmt.Sprintf(`<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages><message name="M" msgtype="M" msgcat="app"><component name="C%d" required="N"/></message></messages>
  <components>%s%s</components>
  <fields>%s</fields>
</fix>`, levels, c0.String(), comps.String(), defs.String())

	if err := loadWithin(t, xml); err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
		t.Fatalf("expected expansion error, got %v", err)
	}
}
