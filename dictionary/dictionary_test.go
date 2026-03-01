package dictionary

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

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
