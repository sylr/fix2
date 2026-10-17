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
		{"xinclude in a message", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><xi:include xmlns:xi="http://www.w3.org/2001/XInclude" href="x.xml"/></sbe:message>`, `<message name="m">: <include>: XInclude`},

		// Names, by their expanded names (§6.1, sbe.xsd).
		{"misspelt attribute of a type", `<type name="x" primitiveType="uint8" presense="optional"/>`, ``, `<type name="x">: unknown attribute "presense"`},
		{"misspelt attribute of a field", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="Reject" id="1"><field name="RefSeqNum" id="2" type="u8" presense="optional"/></sbe:message>`,
			`<message name="Reject">: <field name="RefSeqNum">: unknown attribute "presense"`},
		{"misspelt attribute of a composite's element", `<composite name="c"><type name="a" primitiveType="uint8" sinceVersoin="1"/></composite>`, ``, `<composite name="c">: <type name="a">: unknown attribute "sinceVersoin"`},
		{"attribute of another element", `<enum name="e" encodingType="uint8" presence="optional"><validValue name="a">1</validValue></enum>`, ``, `<enum name="e">: unknown attribute "presence"`},
		{"attribute of a ref", `<type name="u8" primitiveType="uint8"/><composite name="c"><ref name="r" type="u8" description="d"/></composite>`, ``, `<ref name="r">: unknown attribute "description"`},
		{"attribute of a message", ``, `<sbe:message name="m" id="1" dimensionType="groupSizeEncoding"/>`, `<message name="m">: unknown attribute "dimensionType"`},
		{"attribute in the SBE namespace", `<type name="x" sbe:primitiveType="uint8"/>`, ``, `<type name="x">: attribute "primitiveType" in the SBE namespace`},
		{"attribute of an undeclared prefix", `<type name="x" primitiveType="uint8" mbx:venue="b"/>`, ``, `undeclared prefix "mbx"`},
		{"unknown element of a message", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><feild name="a" id="2" type="u8"/></sbe:message>`, `<message name="m">: unexpected element <feild name="a">`},
		{"unknown element of a field", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><field name="a" id="2" type="u8"><type name="b" primitiveType="uint8"/></field></sbe:message>`, `<field name="a">: unexpected element <type name="b">`},
		{"unknown element of the schema", ``, `<sbe:messages/>`, `<messageSchema>: unexpected element <messages>`},
		{"message in no namespace", ``, `<message name="m" id="1"/>`, `<message name="m">: in no namespace, want "http://fixprotocol.io/2016/sbe"`},
		{"type in the SBE namespace", `<sbe:type name="x" primitiveType="uint8"/>`, ``, `<type name="x">: in the SBE namespace: sbe.xsd declares it unqualified`},
		{"element of an undeclared prefix", `<mbx:note/>`, ``, `<mbx:note>: undeclared prefix "mbx"`},
		{"foreign attribute", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><field name="f" id="2" type="u8" xmlns:mbx="https://binance.example/sbe" mbx:x="1"/></sbe:message>`,
			`<message name="m">: <field name="f">: attribute "x" in namespace "https://binance.example/sbe": metadata of another namespace, which sbe.xsd does not allow: -tolerate-foreign-metadata ignores it`},
		{"foreign element", `<type name="u8" primitiveType="uint8"/>`, `<sbe:message name="m" id="1"><mbx:doc xmlns:mbx="https://binance.example/sbe"/></sbe:message>`,
			`<message name="m">: <doc> in namespace "https://binance.example/sbe": metadata of another namespace`},
		{"xml:lang", `<type name="u8" primitiveType="uint8" xml:lang="en"/>`, ``, `<type name="u8">: attribute "lang" in namespace "http://www.w3.org/XML/1998/namespace"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(schema(tc.types, tc.messages)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// The root is SBE 1.0's messageSchema: of its namespace, with its attributes.
func TestLoadRoot(t *testing.T) {
	ok := schema(`<type name="u8" primitiveType="uint8"/>`, ``)
	if _, err := Load(strings.NewReader(ok)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"SBE 2.0's namespace", "http://fixprotocol.io/2016/sbe", "http://fixprotocol.io/2017/sbe",
			`root element <messageSchema> in namespace "http://fixprotocol.io/2017/sbe", want "http://fixprotocol.io/2016/sbe"`},
		{"a release candidate's namespace", "http://fixprotocol.io/2016/sbe", "http://www.fixprotocol.org/ns/simple/1.0",
			`in namespace "http://www.fixprotocol.org/ns/simple/1.0"`},
		{"no namespace", `<sbe:messageSchema xmlns:sbe="http://fixprotocol.io/2016/sbe"`, `<messageSchema xmlns:sbe="http://fixprotocol.io/2016/sbe"`,
			"root element <messageSchema> in no namespace"},
		{"another root", `<sbe:messageSchema xmlns:sbe="http://fixprotocol.io/2016/sbe"`, `<sbe:schema xmlns:sbe="http://fixprotocol.io/2016/sbe"`,
			"root element <schema>, want <messageSchema>"},
		{"a misspelt attribute", `id="1"`, `id="1" bytOrder="bigEndian"`, `<messageSchema>: unknown attribute "bytOrder"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(ok, tc.from, tc.to, 1)
			if strings.HasPrefix(tc.to, "<messageSchema") || strings.HasPrefix(tc.to, "<sbe:schema") {
				src = strings.Replace(src, "</sbe:messageSchema>", "</"+tc.to[1:strings.IndexByte(tc.to, ' ')]+">", 1)
			}
			if _, err := Load(strings.NewReader(src)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// Elements and attributes of other namespaces are errors, sbe.xsd allowing
// none, unless TolerateForeignMetadata: then they are ignored. XML Schema
// instance's attributes are accepted either way.
func TestLoadForeignNamespaces(t *testing.T) {
	plain, err := Load(strings.NewReader(schema(
		`<type name="u8" primitiveType="uint8"/><composite name="c"><type name="a" primitiveType="uint8"/></composite>
		<enum name="e" encodingType="uint8"><validValue name="v">1</validValue></enum>`,
		`<sbe:message name="m" id="1"><field name="f" id="2" type="c"/><group name="g" id="3"><field name="x" id="4" type="e"/></group><data name="d" id="5" type="varData"/></sbe:message>`)))
	if err != nil {
		t.Fatal(err)
	}
	src := schema(
		`<type name="u8" primitiveType="uint8" mbx:unit="lots" xml:lang="en"/><mbx:note>types</mbx:note>
		<composite name="c" mbx:kind="price"><mbx:note/><type name="a" primitiveType="uint8" mbx:scale="2"/></composite>
		<enum name="e" encodingType="uint8"><mbx:note/><validValue name="v" mbx:label="V">1</validValue></enum>`,
		`<mbx:note>before</mbx:note><sbe:message name="m" id="1" mbx:stream="trades"><mbx:doc><field name="nested" id="9" type="nothing"/></mbx:doc>
		<field name="f" id="2" type="c" mbx:x="1"/><group name="g" id="3" mbx:y="2"><field name="x" id="4" type="e"/><mbx:note/></group>
		<data name="d" id="5" type="varData" mbx:z="3"/></sbe:message>`)
	src = strings.Replace(src, `id="1" version="1">`, `id="1" version="1" xmlns:mbx="https://binance.example/sbe" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="http://fixprotocol.io/2016/sbe sbe.xsd" mbx:venue="b">`, 1)
	s, err := LoadWith(strings.NewReader(src), LoadOptions{TolerateForeignMetadata: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(strings.NewReader(src)); err == nil || !strings.Contains(err.Error(), "-tolerate-foreign-metadata") {
		t.Errorf("without TolerateForeignMetadata: %v", err)
	}
	if len(s.Types) != len(plain.Types) || len(s.Messages) != 1 {
		t.Fatalf("%d types, %d messages; want %d, 1", len(s.Types), len(s.Messages), len(plain.Types))
	}
	if m, p := s.Messages[0], plain.Messages[0]; len(m.Fields) != len(p.Fields) || len(m.Groups) != 1 || len(m.Groups[0].Fields) != 1 || len(m.Data) != 1 || m.BlockLength != p.BlockLength {
		t.Errorf("message %+v, want %+v", m.Block, p.Block)
	}
	for i, typ := range s.Types {
		if typ.Name != plain.Types[i].Name || len(typ.Elements) != len(plain.Types[i].Elements) || len(typ.Values) != len(plain.Types[i].Values) {
			t.Errorf("type %+v, want %+v", typ, plain.Types[i])
		}
	}

	// XML Schema instance's location hints are accepted, tolerated or not;
	// xsi:nil and xsi:type, which would change validation, are refused, as
	// any other xsi name.
	for _, c := range []struct{ attr, want string }{
		{`xsi:noNamespaceSchemaLocation="sbe.xsd"`, ""},
		{`xsi:schemaLocation="http://fixprotocol.io/2016/sbe sbe.xsd"`, ""},
		{`xsi:nil="false"`, `<type name="u8">: xsi:nil is not supported`},
		{`xsi:type="sbe:encodedDataType"`, `<type name="u8">: xsi:type is not supported`},
		{`xsi:presense="optional"`, `<type name="u8">: xsi:presense is not an attribute of XML Schema instances`},
	} {
		src := strings.Replace(schema(`<type name="u8" primitiveType="uint8" `+c.attr+`/>`, ``), `id="1" version="1">`,
			`id="1" version="1" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">`, 1)
		for _, opts := range []LoadOptions{{}, {TolerateForeignMetadata: true}} {
			_, err := LoadWith(strings.NewReader(src), opts)
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Errorf("%s, %+v: %v, want an error with %q", c.attr, opts, err, c.want)
			}
		}
	}
}
