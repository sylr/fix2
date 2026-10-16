package sbe

import (
	"strings"
	"testing"
)

func TestLoadExamples(t *testing.T) {
	s, err := LoadFile("testdata/examples.xml")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != 91 || s.Version != 0 || s.BigEndian || len(s.Messages) != 3 || s.Header.Size != 8 {
		t.Fatalf("schema %d v%d big=%v, %d messages, header %d bytes", s.ID, s.Version, s.BigEndian, len(s.Messages), s.Header.Size)
	}
	nos := s.Messages[0]
	if nos.Name != "NewOrderSingle" || nos.ID != 99 || nos.BlockLength != 54 || len(nos.Fields) != 9 {
		t.Fatalf("NewOrderSingle: %+v", nos.Block)
	}
	price := nos.Fields[7]
	if price.Name != "Price" || price.Offset != 38 || price.Type.Size != 8 || price.Presence != Required {
		t.Errorf("Price: %+v", price)
	}
	if m := price.Type.Elements[0].Type; m.Presence != Optional || m.Null != "-9223372036854775808" {
		t.Errorf("Price mantissa: %+v", m)
	}
	er := s.Messages[1]
	if len(er.Groups) != 1 || er.Groups[0].BlockLength != 12 || er.Groups[0].MaxEntries != 65534 || er.Groups[0].MaxEntriesSet {
		t.Errorf("FillsGrp: %+v", er.Groups)
	}
	bmr := s.Messages[2]
	if len(bmr.Data) != 1 || bmr.Data[0].MaxLength != 65534 {
		t.Errorf("Text: %+v", bmr.Data)
	}
	if side := nos.Fields[3].Type; side.Kind != KindEnum || side.Values[0].Value != "49" {
		t.Errorf("Side: %+v", side)
	}
}

func TestLoadFIXP(t *testing.T) {
	s, err := LoadFile("../../../fixp/internal/codec/SBEschemaForFIXP.xml")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != 2748 || len(s.Messages) != 19 {
		t.Fatalf("schema %d, %d messages", s.ID, len(s.Messages))
	}
	est := s.Messages[4]
	if est.Name != "Establish" || est.BlockLength != 36 || est.Fields[3].Presence != Optional || len(est.Data) != 1 {
		t.Fatalf("Establish: %+v", est.Block)
	}
}

// schema wraps types and messages in a minimal schema with a header.
func schema(types, messages string) string {
	return `<sbe:messageSchema xmlns:sbe="http://fixprotocol.io/2016/sbe" id="1" version="1"><types>
<composite name="messageHeader"><type name="blockLength" primitiveType="uint16"/><type name="templateId" primitiveType="uint16"/><type name="schemaId" primitiveType="uint16"/><type name="version" primitiveType="uint16"/></composite>
<composite name="groupSizeEncoding"><type name="blockLength" primitiveType="uint16"/><type name="numInGroup" primitiveType="uint16"/></composite>
<composite name="varData"><type name="length" primitiveType="uint16"/><type name="data" primitiveType="uint8" length="0"/></composite>
` + types + `</types>` + messages + `</sbe:messageSchema>`
}

func TestLoadRejects(t *testing.T) {
	for _, tc := range []struct {
		name, types, messages, want string
	}{
		{"unknown primitive", `<type name="x" primitiveType="int128"/>`, ``, "not an SBE primitive"},
		{"nullValue on required", `<type name="x" primitiveType="uint8" nullValue="1"/>`, ``, "nullValue on a required type"},
		{"nullValue out of range", `<type name="x" primitiveType="uint8" presence="optional" nullValue="256"/>`, ``, "nullValue"},
		{"constant without value", `<type name="x" primitiveType="uint8" presence="constant"/>`, ``, "constant without a value"},
		{"duplicate type", `<type name="x" primitiveType="uint8"/><type name="x" primitiveType="uint8"/>`, ``, "duplicate encoding name"},
		{"cycle", `<composite name="a"><ref name="b" type="b"/></composite><composite name="b"><ref name="a" type="a"/></composite>`, ``, "refers to itself"},
		{"set over char", `<set name="s" encodingType="char"><choice name="a">0</choice></set>`, ``, "not an unsigned integer"},
		{"choice beyond its bits", `<set name="s" encodingType="uint8"><choice name="a">8</choice></set>`, ``, "out of a uint8"},
		{"enum without values", `<enum name="e" encodingType="uint8"></enum>`, ``, "no validValue"},
		{"overlapping offset", `<composite name="c"><type name="a" primitiveType="uint32"/><type name="b" primitiveType="uint8" offset="2"/></composite>`, ``, "overlaps"},
		{"sinceVersion beyond the schema", `<type name="x" primitiveType="uint8" sinceVersion="2"/>`, ``, "beyond the schema's version"},
		{"field after group", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><group name="g" id="2"><field name="a" id="3" type="u8"/></group><field name="b" id="4" type="u8"/></sbe:message>`, "after a group"},
		{"group after data", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><data name="d" id="2" type="varData"/><group name="g" id="3"><field name="a" id="4" type="u8"/></group></sbe:message>`, "group \"g\" after data"},
		{"blockLength too small", `<type name="u32" primitiveType="uint32"/>`, `<sbe:message name="m" id="1" blockLength="2"><field name="a" id="2" type="u32"/></sbe:message>`, "less than its fields"},
		{"duplicate message id", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><field name="a" id="2" type="u8"/></sbe:message><sbe:message name="n" id="1"><field name="a" id="2" type="u8"/></sbe:message>`, "duplicate message"},
		{"two groups of a name", `<type name="u8" primitiveType="uint8"/><type name="u16" primitiveType="uint16"/>`, `<sbe:message name="m" id="1"><group name="g" id="2"><field name="a" id="3" type="u8"/></group></sbe:message><sbe:message name="n" id="2"><group name="g" id="2"><field name="a" id="3" type="u16"/></group></sbe:message>`, "different group of the same name"},
		{"two groups of a name, their members of different versions", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><group name="g" id="2"><field name="a" id="3" type="u8"/></group></sbe:message><sbe:message name="n" id="2"><group name="g" id="2"><field name="a" id="3" type="u8" sinceVersion="1"/></group></sbe:message>`, "different group of the same name"},
		{"data shorter than it can be", `<composite name="d2"><type name="length" primitiveType="uint8" minValue="5" maxValue="3"/><type name="data" primitiveType="uint8" length="0"/></composite>`, `<sbe:message name="m" id="1"><data name="d" id="2" type="d2"/></sbe:message>`, "its length allows 5 to 3 bytes"},
		{"valueRef to nothing", `<type name="x" primitiveType="uint8" presence="constant" valueRef="E.v"/>`, ``, "valueRef"},
		{"data of a scalar", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><data name="d" id="2" type="u8"/></sbe:message>`, "not a composite of an unsigned length"},
		{"variable-length field", ``, `<sbe:message name="m" id="1"><field name="d" id="2" type="varData"/></sbe:message>`, "variable-length"},
		{"optional composite with a required first element", `<composite name="c"><type name="a" primitiveType="int32"/></composite>`, `<sbe:message name="m" id="1"><field name="f" id="2" type="c" presence="optional"/></sbe:message>`, "first element must be an optional scalar"},
		{"bad name", `<type name="a b" primitiveType="uint8"/>`, ``, "letters, digits and underscores"},
		{"xinclude", `<xi:include xmlns:xi="http://www.w3.org/2001/XInclude" href="x.xml"/>`, ``, "XInclude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(schema(tc.types, tc.messages)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want an error with %q", err, tc.want)
			}
		})
	}
}
