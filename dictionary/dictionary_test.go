package dictionary

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"
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
	if nos.body.get(55) == nil {
		t.Fatal("Symbol (55) should be in NewOrderSingle fields via Instrument component")
	}
}

func TestResolve_GroupCounterAndChildren(t *testing.T) {
	d := loadTestDict(t)
	nos := d.Messages["D"]

	// NoAllocs counter (tag 78) should be present.
	if nos.body.get(78) == nil {
		t.Fatal("NoAllocs counter (78) should be in NewOrderSingle fields")
	}
	// AllocAccount (tag 79) should be present (group child).
	if nos.body.get(79) == nil {
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

	// Layout keeps fields and the nested group counter in XML order.
	if got, want := legs.Layout, []int{600, 624, 687, 539}; !slices.Equal(got, want) {
		t.Errorf("NoLegs Layout = %v, want %v", got, want)
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
	var required []int
	for _, t := range d.Messages["M"].requiredTags {
		required = append(required, t.tag)
	}
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

func TestLoad_SizeBoundary(t *testing.T) {
	const head = `<fix type="FIX" major="4" minor="4"><header/><trailer/><messages/><components/><fields/><!--`
	const tail = `--></fix>`
	dict := func(size int) string {
		return head + strings.Repeat("a", size-len(head)-len(tail)) + tail
	}
	if _, err := Load(strings.NewReader(dict(maxDictSize))); err != nil {
		t.Errorf("dictionary of exactly maxDictSize bytes: %v", err)
	}
	// Valid XML that decodes successfully but is one byte over the limit.
	if _, err := Load(strings.NewReader(dict(maxDictSize + 1))); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("dictionary of maxDictSize+1 bytes: got %v, want size error", err)
	}
}

func TestLoad_GroupConditionTrackingCharged(t *testing.T) {
	// A group of 30,000 fields inside 40 nested optional components: every
	// group child is recorded in all 40 enclosing conditions.
	const fields, levels = 30_000, 40
	var defs, c0, comps strings.Builder
	defs.WriteString(`<field number="998" name="NoG" type="NUMINGROUP"/><field number="999" name="R" type="STRING"/>`)
	c0.WriteString(`<component name="C0"><field name="R" required="Y"/><group name="NoG" required="N">`)
	for i := range fields {
		fmt.Fprintf(&defs, `<field number="%d" name="F%d" type="STRING"/>`, 1000+i, i)
		fmt.Fprintf(&c0, `<field name="F%d" required="N"/>`, i)
	}
	c0.WriteString(`</group></component>`)
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

func TestResolver_ChargesBeforeRecording(t *testing.T) {
	field := func(name string) xmlItem { return xmlItem{XMLName: xml.Name{Local: "field"}, Name: name} }
	group := xmlItem{XMLName: xml.Name{Local: "group"}, Name: "NoG", xmlBody: xmlBody{Items: []xmlItem{field("X"), field("Y")}}}
	r := &resolver{
		byName:   map[string]*FieldDef{"NoG": {Tag: 100}, "X": {Tag: 101}, "Y": {Tag: 102}},
		compMap:  map[string]*xmlComponent{},
		visiting: map[string]bool{},
	}
	// Inside one optional component, the group costs 1 visit + 2 field visits
	// + 2 copied refs, then 1 x (1 counter + 2 children) condition updates.
	// Leave budget for everything but the condition updates.
	r.visits = maxVisits - 5
	b := &resolvedBody{conds: []rawCondition{{component: "C", parent: -1, counts: map[int]int{}}}}

	if err := r.walk([]xmlItem{group}, b, 0); err == nil || !strings.Contains(err.Error(), "expansion exceeds") {
		t.Fatalf("expected expansion error, got %v", err)
	}
	if len(b.conds[0].counts) != 0 || len(b.all) != 0 {
		t.Errorf("work done before being charged: counts=%v all=%v", b.conds[0].counts, b.all)
	}
}

func TestLoad_LongNames(t *testing.T) {
	// dict defines field F (tag 1), group counter G (tag 2) and component C,
	// with the names overridable to exercise definitions and references.
	type names struct{ fieldDef, fieldRef, groupDef, groupRef, compDef, compRef string }
	dict := func(n names) string {
		return fmt.Sprintf(`<fix type="FIX" major="4" minor="4">
  <header/><trailer/>
  <messages><message name="M" msgtype="M" msgcat="app"><component name="%s" required="N"/></message></messages>
  <components><component name="%s"><field name="%s" required="N"/><group name="%s" required="N"><field name="%s" required="N"/></group></component></components>
  <fields><field number="1" name="%s" type="STRING"/><field number="2" name="%s" type="NUMINGROUP"/></fields>
</fix>`, n.compRef, n.compDef, n.fieldRef, n.groupRef, n.fieldRef, n.fieldDef, n.groupDef)
	}
	ok := names{"F", "F", "G", "G", "C", "C"}
	long := strings.Repeat("N", maxNameLen+1)
	// Distinct names of exactly maxNameLen bytes.
	atLimit := func(prefix string) string { return prefix + strings.Repeat("x", maxNameLen-len(prefix)) }
	f, g, c := atLimit("F"), atLimit("G"), atLimit("C")

	for name, n := range map[string]names{
		"field definition":     {long, "F", "G", "G", "C", "C"},
		"field reference":      {"F", long, "G", "G", "C", "C"},
		"group reference":      {"F", "F", "G", long, "C", "C"},
		"component definition": {"F", "F", "G", "G", long, long},
		"component reference":  {"F", "F", "G", "G", "C", long},
	} {
		t.Run(name, func(t *testing.T) {
			if err := loadWithin(t, dict(n)); err == nil || !strings.Contains(err.Error(), "exceeds 256") {
				t.Fatalf("expected name length error, got %v", err)
			}
		})
	}
	if err := loadWithin(t, dict(ok)); err != nil {
		t.Fatalf("short names: %v", err)
	}
	// Exactly maxNameLen bytes is accepted everywhere.
	if err := loadWithin(t, dict(names{f, f, g, g, c, c})); err != nil {
		t.Fatalf("names of exactly %d bytes: %v", maxNameLen, err)
	}
}

// TestLoad_EnvelopeNotCopiedPerMessage guards against per-message work
// proportional to the header size: a 5,000-field header with 2,000 empty
// messages must load without allocating per-message copies of the header.
func TestLoad_EnvelopeNotCopiedPerMessage(t *testing.T) {
	const headerFields, messages = 5_000, 2_000
	var defs, header, msgs strings.Builder
	for i := range headerFields {
		fmt.Fprintf(&defs, `<field number="%d" name="H%d" type="STRING"/>`, 1000+i, i)
		fmt.Fprintf(&header, `<field name="H%d" required="N"/>`, i)
	}
	for i := range messages {
		fmt.Fprintf(&msgs, `<message name="M%d" msgtype="M%d" msgcat="app"/>`, i, i)
	}
	xml := fmt.Sprintf(`<fix type="FIX" major="4" minor="4"><header>%s</header><trailer/><messages>%s</messages><components/><fields>%s</fields></fix>`,
		header.String(), msgs.String(), defs.String())

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := Load(strings.NewReader(xml)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	runtime.ReadMemStats(&after)
	// Copying the 5,000-entry header counts for each of the 2,000 messages
	// allocates hundreds of MB; without it Load needs a few MB.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<20 {
		t.Errorf("Load allocated %d MB", alloc>>20)
	}
}

func TestLoad_HeaderGroups(t *testing.T) {
	d, err := LoadFile("../cmd/fixgen/testdata/FIXT11.xml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(d.HeaderGroups) != 1 {
		t.Fatalf("got %d header groups, want 1", len(d.HeaderGroups))
	}
	if g := d.HeaderGroups[0]; g.Name != "NoHops" || g.CounterTag != 627 || g.DelimTag != 628 {
		t.Errorf("header group = %s counter %d delim %d, want NoHops 627 628", g.Name, g.CounterTag, g.DelimTag)
	}
	if len(d.TrailerGroups) != 0 {
		t.Errorf("got %d trailer groups, want 0", len(d.TrailerGroups))
	}
}

func TestLoad_ReferenceChildrenRejected(t *testing.T) {
	// Only groups have children. A million elements inside a field reference
	// used to be decoded, then ignored by the resolver.
	const fields = `<fields><field number="1" name="F" type="STRING"/></fields>`
	dict := func(body string) string {
		return `<fix major="4" minor="4"><header/><trailer/><messages><message name="M" msgtype="M">` +
			body + `</message></messages><components><component name="C"><field name="F"/></component></components>` + fields + `</fix>`
	}
	for name, xml := range map[string]string{
		"field":     dict(`<field name="F">` + strings.Repeat("<field/>", 1_000_000) + `</field>`),
		"component": dict(`<component name="C"><group name="F"/></component>`),
		"unknown":   dict(`<x name="F"><field name="F"/></x>`),
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			allocs := testing.AllocsPerRun(1, func() { _, err = Load(strings.NewReader(xml)) })
			if err == nil || !strings.Contains(err.Error(), "only groups have children") {
				t.Fatalf("expected children error, got %v", err)
			}
			if allocs > 1000 {
				t.Errorf("Load allocated %v times before rejecting the dictionary", allocs)
			}
		})
	}
	// Text inside a reference is not an element and stays accepted.
	if _, err := Load(strings.NewReader(dict(`<field name="F"> </field>`))); err != nil {
		t.Errorf("whitespace inside a reference: %v", err)
	}
}

func TestLoad_ElementLimit(t *testing.T) {
	// Unknown elements are skipped, but still counted.
	dict := func(elements int) string {
		return `<fix major="4" minor="4">` + strings.Repeat("<x/>", elements-1) + `</fix>`
	}
	if _, err := Load(strings.NewReader(dict(maxElements))); err != nil {
		t.Errorf("maxElements elements: %v", err)
	}
	if _, err := Load(strings.NewReader(dict(maxElements + 1))); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("maxElements+1 elements: got %v, want element limit error", err)
	}
	// Nested unknown elements are skipped without recursion.
	nested := `<fix>` + strings.Repeat("<x>", maxElements) + strings.Repeat("</x>", maxElements) + `</fix>`
	if _, err := Load(strings.NewReader(nested)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("nested elements: got %v, want element limit error", err)
	}
}

func TestLoad_GroupNestingLimit(t *testing.T) {
	// Groups nested in the XML itself are limited while decoding.
	dict := func(levels int) string {
		return `<fix major="4" minor="4"><header/><trailer/><messages><message name="M" msgtype="M">` +
			strings.Repeat(`<group name="NoG">`, levels) + `<field name="F"/>` + strings.Repeat(`</group>`, levels) +
			`</message></messages><components/><fields><field number="1" name="F" type="STRING"/>` +
			`<field number="2" name="NoG" type="NUMINGROUP"/></fields></fix>`
	}
	if _, err := Load(strings.NewReader(dict(maxDepth + 1))); err == nil || !strings.Contains(err.Error(), "nested deeper than") {
		t.Errorf("maxDepth+1 groups: got %v, want depth error", err)
	}
	if _, err := Load(strings.NewReader(dict(100_000))); err == nil || !strings.Contains(err.Error(), "nested deeper than") {
		t.Errorf("100,000 groups: got %v, want depth error", err)
	}
}

func TestLoad_MalformedXML(t *testing.T) {
	for name, xml := range map[string]string{
		"empty":         ``,
		"unclosed root": `<fix major="4" minor="4"><header/>`,
		"mismatched":    `<fix><header></trailer></fix>`,
		"bad number":    `<fix><fields><field number="x" name="F" type="STRING"/></fields></fix>`,
	} {
		if _, err := Load(strings.NewReader(xml)); err == nil {
			t.Errorf("%s: Load succeeded", name)
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
	d, err := Load(strings.NewReader(xml))
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
		var ve *ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, ErrRequiredTag) || ve.Tag != tt.missing {
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
	d, err := Load(strings.NewReader(xml))
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
		var ve *ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, ErrRequiredTag) || ve.Tag != tt.missing {
			t.Errorf("%s: got %v, want required tag %d missing", tt.name, err, tt.missing)
		}
	}
}

func TestValidate_HeaderNoHops(t *testing.T) {
	d, err := LoadFile("../cmd/fixgen/testdata/FIXT11.xml")
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
	if spec := d.Messages["0"].specs; len(spec) != 1 || spec[0].Counter != 627 || spec[0].Delim != 628 {
		t.Fatalf("Heartbeat specs = %+v", spec)
	}
}

func TestLoad_EnvelopeSpecsNotPerMessage(t *testing.T) {
	// Header groups used to be copied into every message's specs: messages
	// x header groups pointers, outside the resolver budget.
	alloc := func(n int) uint64 {
		var header, msgs, fields strings.Builder
		fields.WriteString(`<field number="35" name="MsgType" type="STRING"/><field number="1" name="F" type="STRING"/>`)
		for i := range n {
			fmt.Fprintf(&header, `<group name="NoH%d"><field name="F"/></group>`, i)
			fmt.Fprintf(&msgs, `<message name="M%d" msgtype="M%d"><field name="F"/></message>`, i, i)
			fmt.Fprintf(&fields, `<field number="%d" name="NoH%d" type="NUMINGROUP"/>`, 1000+i, i)
		}
		xml := `<fix major="4" minor="4"><header>` + header.String() + `</header><trailer/><messages>` + msgs.String() +
			`</messages><components/><fields>` + fields.String() + `</fields></fix>`
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if _, err := Load(strings.NewReader(xml)); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := alloc(1000), alloc(4000)
	if ratio := float64(large) / float64(small); ratio > 6 {
		t.Errorf("4x the messages and header groups allocated %.1fx more (%d vs %d bytes), want about 4x", ratio, large, small)
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
	d, err := Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tests := []struct {
		name string
		body string
		err  error // nil when valid
		tag  int
	}{
		{"unknown tag with a tag 0 field declared", "35=M\x01100=f\x01999=x\x01", ErrUnexpectedTag, 999},
		{"LENGTH of an allowed DATA field, undeclared", "35=M\x01100=f\x01160=1\x01354=2\x01355=ab\x01", nil, 0},
		{"F only in a group led by undeclared LENGTH", "35=M\x01160=1\x01354=2\x01355=ab\x01100=in\x01", ErrRequiredTag, 100},
		{"F only in a group led by LENGTH declared later", "35=M\x01161=1\x0195=2\x0196=ab\x01100=in\x01", ErrRequiredTag, 100},
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
		var ve *ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, tt.err) || ve.Tag != tt.tag {
			t.Errorf("%s: got %v, want %v for tag %d", tt.name, err, tt.err, tt.tag)
		}
	}
}

func TestValidate_MalformedCounts(t *testing.T) {
	// Entries after a count of zero, a negative or non-numeric count, or
	// beyond the count are not group entries, as GroupsOf reads them: their
	// fields are top level. Nothing panics.
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
	d, err := Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, count := range []string{"0", "-1", "x", "99999999999999999999"} {
		msg := parseMsg(t, buildFIXMsg("35=M\x01101="+count+"\x01102=d\x01100=f\x01"))
		if err := d.Validate(&msg); err != nil {
			t.Errorf("count %s: %v", count, err)
		}
	}
	// Undercounted: the second entry's F is top level.
	msg := parseMsg(t, buildFIXMsg("35=M\x01101=1\x01102=a\x01100=in\x01102=b\x01100=f\x01"))
	if err := d.Validate(&msg); err != nil {
		t.Errorf("undercounted: %v", err)
	}
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
	d, err := Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	msg := parseMsg(t, buildFIXMsg("35=M\x01101=1\x01102=d\x0195=2\x0196=ab\x01100=in\x01"))
	var ve *ValidationError
	if err := d.Validate(&msg); !errors.As(err, &ve) || !errors.Is(err, ErrRequiredTag) || ve.Tag != 100 {
		t.Errorf("got %v, want required tag 100 missing", err)
	}
	// Two entries with their DATA field, and F direct: valid.
	msg = parseMsg(t, buildFIXMsg("35=M\x01100=f\x01101=2\x01102=d\x0195=2\x0196=ab\x01102=e\x0195=1\x0196=c\x01"))
	if err := d.Validate(&msg); err != nil {
		t.Errorf("valid message: %v", err)
	}
	// A pair the message declares neither field of stays unexpected.
	msg = parseMsg(t, buildFIXMsg("35=M\x01100=f\x0193=1\x0189=x\x01"))
	if err := d.Validate(&msg); !errors.Is(err, ErrUnexpectedTag) {
		t.Errorf("Signature pair: got %v, want unexpected tag", err)
	}
}

func TestValidate_ErrorMessages(t *testing.T) {
	d := loadTestDict(t)
	const order = "11=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"
	for _, tc := range []struct{ body, want string }{
		{"35=D\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01" + order, "fix2: required header tag 49 missing"},
		{"35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01", "fix2: required tag 11 missing for message NewOrderSingle"},
		{"35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01" + order + "999=x\x01", "fix2: tag 999 not allowed in message NewOrderSingle"},
		{"35=Z\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x01", `fix2: unknown MsgType "Z"`},
	} {
		msg := parseMsg(t, buildFIXMsg(tc.body))
		if err := d.Validate(&msg); err == nil || err.Error() != tc.want {
			t.Errorf("got %v, want %q", err, tc.want)
		}
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

func TestTagTable(t *testing.T) {
	entries := map[int]tagEntry{}
	for _, tag := range []int{-7, 0, 1, 8, 1 << 30, -(1 << 30)} {
		entries[tag] = tagEntry{slot: int32(tag & 0xff)}
	}
	// Tags sharing their low bits, as a crafted dictionary could choose.
	for i := range 1000 {
		entries[i<<20] = tagEntry{slot: int32(i)}
	}
	tt := newTagTable(entries)
	for tag, want := range entries {
		if e := tt.get(tag); e == nil || e.slot != want.slot || e.tag != tag {
			t.Fatalf("get(%d) = %+v, want slot %d", tag, e, want.slot)
		}
	}
	for _, tag := range []int{2, -1, 1<<30 + 1, 1001 << 20} {
		if e := tt.get(tag); e != nil {
			t.Errorf("get(%d) = %+v, want nil", tag, e)
		}
	}
	if e := newTagTable(nil); e.get(1) != nil {
		t.Error("empty table found a tag")
	}
}

// slotsDict returns a dictionary whose message M requires n fields, tags 50000
// to 50000+n-1, after a header requiring MsgType: n+1 presence slots.
func slotsDict(t *testing.T, n int) *Dict {
	t.Helper()
	var refs, defs strings.Builder
	for i := range n {
		fmt.Fprintf(&refs, `<field name="F%d" required="Y"/>`, i)
		fmt.Fprintf(&defs, `<field number="%d" name="F%d" type="STRING"/>`, 50000+i, i)
	}
	d, err := Load(strings.NewReader(`<fix type="FIX" major="4" minor="4">
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
		var ve *ValidationError
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
	d, err := Load(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct {
		body string
		err  error
		tag  int
	}{
		{"35=M\x0149=s\x01", nil, 0},
		{"35=M\x01", ErrRequiredTag, 49}, // header first
		{"35=M\x0149=s\x01100=LONG\x01", nil, 0},
		{"35=M\x0149=s\x01100=S\x01", nil, 0},
		{"35=M\x0149=s\x01100=LONGER\x01", ErrInvalidEnumValue, 100},
		{"35=M\x0149=s\x01100=L\x01", ErrInvalidEnumValue, 100},
		// Missing required tag, unexpected tag and invalid enum: the
		// required tag is reported.
		{"35=M\x01999=x\x01100=Q\x01", ErrRequiredTag, 49},
		// Unexpected tag before invalid enum: the first in field order.
		{"35=M\x0149=s\x01999=x\x01100=Q\x01", ErrUnexpectedTag, 999},
		{"35=M\x0149=s\x01100=Q\x01999=x\x01", ErrInvalidEnumValue, 100},
	} {
		msg := parseMsg(t, buildFIXMsg(tc.body))
		err := d.Validate(&msg)
		var ve *ValidationError
		if tc.err == nil && err != nil || tc.err != nil && (!errors.Is(err, tc.err) || !errors.As(err, &ve) || ve.Tag != tc.tag) {
			t.Errorf("%q: got %v, want %v for tag %d", tc.body, err, tc.err, tc.tag)
		}
	}
	// Changing the loaded dictionary does not change validation.
	d.Fields[100].Enums["LONGER"] = "added"
	msg := parseMsg(t, buildFIXMsg("35=M\x0149=s\x01100=LONGER\x01"))
	if err := d.Validate(&msg); !errors.Is(err, ErrInvalidEnumValue) {
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
	// More header groups than maxSharedSpecs: their specs are kept once, and
	// a message without groups of its own uses them as they are.
	var header, fields strings.Builder
	fields.WriteString(`<field number="35" name="MsgType" type="STRING"/><field number="100" name="F" type="STRING"/>`)
	for i := range maxSharedSpecs + 1 {
		fmt.Fprintf(&header, `<group name="NoH%d"><field name="F"/></group>`, i)
		fmt.Fprintf(&fields, `<field number="%d" name="NoH%d" type="NUMINGROUP"/>`, 1000+2*i, i)
	}
	d, err := Load(strings.NewReader(`<fix major="4" minor="4"><header><field name="MsgType" required="Y"/>` + header.String() +
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

func TestLoad_DuplicateFields(t *testing.T) {
	for name, fields := range map[string]string{
		"number": `<field number="1" name="A" type="STRING"/><field number="1" name="B" type="STRING"/>`,
		"name":   `<field number="1" name="A" type="STRING"/><field number="2" name="A" type="STRING"/>`,
	} {
		_, err := Load(strings.NewReader(`<fix major="4" minor="4"><header/><trailer/><messages/><components/><fields>` + fields + `</fields></fix>`))
		if err == nil || !strings.Contains(err.Error(), "the same "+name) {
			t.Errorf("duplicate %s: got %v", name, err)
		}
	}
}

func TestNew_Idempotent(t *testing.T) {
	d := loadTestDict(t)
	nos := d.Messages["D"]
	enums, required := len(d.enumChecks), len(nos.requiredTags)
	New(d)
	if len(d.enumChecks) != enums || len(nos.requiredTags) != required {
		t.Errorf("after a second New: %d enum checks, %d required tags; want %d and %d", len(d.enumChecks), len(nos.requiredTags), enums, required)
	}
	msg := parseMsg(t, buildFIXMsg("35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20231001-12:00:00\x0111=ORD1\x0154=1\x0160=20231001-12:00:00\x0140=1\x0155=AAPL\x01"))
	if err := d.Validate(&msg); err != nil {
		t.Errorf("after a second New: %v", err)
	}
}

func TestNew_IdempotentWithConditionsAndEnvelopeGroups(t *testing.T) {
	// Five header groups (more than maxSharedSpecs) and an optional
	// component in the body requiring B when C is present.
	var header, fields strings.Builder
	fields.WriteString(`<field number="35" name="MsgType" type="STRING"/><field number="100" name="F" type="STRING"/>` +
		`<field number="102" name="B" type="STRING"/><field number="103" name="C" type="STRING"/>`)
	for i := range maxSharedSpecs + 1 {
		fmt.Fprintf(&header, `<group name="NoH%d"><field name="F"/></group>`, i)
		fmt.Fprintf(&fields, `<field number="%d" name="NoH%d" type="NUMINGROUP"/>`, 1000+2*i, i)
	}
	d, err := Load(strings.NewReader(`<fix major="4" minor="4"><header><field name="MsgType" required="Y"/>` + header.String() +
		`</header><trailer/><messages><message name="M" msgtype="M"><field name="F" required="Y"/><component name="Opt" required="N"/></message></messages>` +
		`<components><component name="Opt"><field name="B" required="Y"/><field name="C" required="N"/></component></components><fields>` + fields.String() + `</fields></fix>`))
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		for body, want := range map[string]error{
			"35=M\x011000=1\x01100=g\x01100=f\x01": nil,
			"35=M\x011000=1\x01100=g\x01":          ErrRequiredTag, // F only in a header group
			"35=M\x01100=f\x01103=c\x01":           ErrRequiredTag, // Opt present, B missing
			"35=M\x01100=f\x01103=c\x01102=b\x01":  nil,
			"35=M\x01100=f\x01999=x\x01":           ErrUnexpectedTag,
		} {
			msg := parseMsg(t, buildFIXMsg(body))
			if err := d.Validate(&msg); want == nil && err != nil || want != nil && !errors.Is(err, want) {
				t.Errorf("%s: %q: got %v, want %v", when, body, err, want)
			}
		}
	}
	check("after Load")
	m := d.Messages["M"]
	conds := len(m.conds)
	New(d)
	if len(m.conds) != conds || len(d.envelopeSpecs) != maxSharedSpecs+1 {
		t.Errorf("after a second New: %d conditions, %d envelope specs", len(m.conds), len(d.envelopeSpecs))
	}
	check("after a second New")
	// Down to maxSharedSpecs header groups: their specs move into the
	// message's.
	d.HeaderGroups = d.HeaderGroups[:maxSharedSpecs]
	New(d)
	if d.envelopeSpecs != nil || len(m.specs) != maxSharedSpecs {
		t.Errorf("with %d header groups: envelope specs %d, message specs %d", maxSharedSpecs, len(d.envelopeSpecs), len(m.specs))
	}
	check("with fewer header groups")
}
