package xmldict

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/sylr/fix2/internal/dicttest"
)

// testOrchestraXML is an Orchestra repository using what Load translates:
// code sets, the standard header and trailer, an optional component with a
// required field, groups nested by reference, a forbidden reference,
// annotations and rules, and a scenario.
const testOrchestraXML = `<?xml version="1.0" encoding="UTF-8"?>
<fixr:repository xmlns:fixr="http://fixprotocol.io/2020/orchestra/repository" xmlns:dcterms="http://purl.org/dc/terms/" name="FIX.4.4" version="FIX.4.4_EP311">
  <fixr:metadata><dcterms:title>Test</dcterms:title></fixr:metadata>
  <fixr:datatypes><fixr:datatype name="String"/></fixr:datatypes>
  <fixr:codeSets>
    <fixr:codeSet id="54" name="SideCodeSet" type="char">
      <fixr:code id="54001" name="Buy" value="1"><fixr:annotation><fixr:documentation>Buy</fixr:documentation></fixr:annotation></fixr:code>
      <fixr:code id="54005" name="SellShort" value="5"/>
    </fixr:codeSet>
    <fixr:codeSet id="54" name="SideCodeSet" scenario="Cross" type="char">
      <fixr:code id="54008" name="Cross" value="8"/>
    </fixr:codeSet>
    <fixr:codeSet id="221" name="BenchmarkCurveNameCodeSet" type="String">
      <fixr:code id="221003" name="Euribor" value="Euribor" deprecated="FIX.5.0SP2"/>
      <fixr:code id="221015" name="EURIBOR" value="EURIBOR"/>
    </fixr:codeSet>
  </fixr:codeSets>
  <fixr:fields>
    <fixr:field id="8" name="BeginString" type="String"/>
    <fixr:field id="9" name="BodyLength" type="Length"/>
    <fixr:field id="35" name="MsgType" type="String"/>
    <fixr:field id="49" name="SenderCompID" type="String"/>
    <fixr:field id="56" name="TargetCompID" type="String"/>
    <fixr:field id="34" name="MsgSeqNum" type="SeqNum"/>
    <fixr:field id="52" name="SendingTime" type="UTCTimestamp"/>
    <fixr:field id="10" name="CheckSum" type="String"/>
    <fixr:field id="112" name="TestReqID" type="String"><fixr:annotation><fixr:documentation>ID</fixr:documentation></fixr:annotation></fixr:field>
    <fixr:field id="11" name="ClOrdID" type="String"/>
    <fixr:field id="54" name="Side" type="SideCodeSet"/>
    <fixr:field id="55" name="Symbol" type="String"/>
    <fixr:field id="48" name="SecurityID" type="String"/>
    <fixr:field id="22" name="SecurityIDSource" type="String"/>
    <fixr:field id="221" name="BenchmarkCurveName" type="BenchmarkCurveNameCodeSet"/>
    <fixr:field id="453" name="NoPartyIDs" type="NumInGroup"/>
    <fixr:field id="448" name="PartyID" type="String"/>
    <fixr:field id="452" name="PartyRole" type="int"/>
    <fixr:field id="802" name="NoPartySubIDs" type="NumInGroup"/>
    <fixr:field id="523" name="PartySubID" type="String"/>
  </fixr:fields>
  <fixr:components>
    <fixr:component id="1024" name="StandardHeader" category="Session">
      <fixr:fieldRef id="8" presence="required"/>
      <fixr:fieldRef id="9" presence="required"/>
      <fixr:fieldRef id="35" presence="required"/>
      <fixr:fieldRef id="49" presence="required"/>
      <fixr:fieldRef id="56" presence="required"/>
      <fixr:fieldRef id="34" presence="required"/>
      <fixr:fieldRef id="52" presence="required"/>
    </fixr:component>
    <fixr:component id="1025" name="StandardTrailer" category="Session">
      <fixr:fieldRef id="10" presence="required"/>
    </fixr:component>
    <fixr:component id="1003" name="Instrument" category="Common">
      <fixr:fieldRef id="55" presence="required"><fixr:annotation><fixr:documentation>Symbol</fixr:documentation></fixr:annotation></fixr:fieldRef>
      <fixr:fieldRef id="48"/>
      <fixr:fieldRef id="22"><fixr:rule name="WhenSecurityID" presence="required"><fixr:when>SecurityID != nil</fixr:when></fixr:rule></fixr:fieldRef>
      <fixr:fieldRef id="221" presence="forbidden"/>
    </fixr:component>
  </fixr:components>
  <fixr:groups>
    <fixr:group id="1012" name="Parties" category="Common">
      <fixr:numInGroup id="453"/>
      <fixr:fieldRef id="448" presence="required"/>
      <fixr:fieldRef id="452"/>
      <fixr:groupRef id="1062"/>
    </fixr:group>
    <fixr:group id="1062" name="PtysSubGrp" category="Common">
      <fixr:numInGroup id="802"/>
      <fixr:fieldRef id="523"/>
    </fixr:group>
  </fixr:groups>
  <fixr:messages>
    <fixr:message id="1" name="Heartbeat" msgType="0" category="Session">
      <fixr:structure>
        <fixr:componentRef id="1024" presence="required"/>
        <fixr:fieldRef id="112"/>
        <fixr:componentRef id="1025" presence="required"/>
      </fixr:structure>
      <fixr:annotation><fixr:documentation>Heartbeat</fixr:documentation></fixr:annotation>
    </fixr:message>
    <fixr:message id="14" name="NewOrderSingle" msgType="D" category="SingleGeneralOrderHandling">
      <fixr:structure>
        <fixr:componentRef id="1024" presence="required"/>
        <fixr:fieldRef id="11" presence="required"/>
        <fixr:groupRef id="1012"/>
        <fixr:fieldRef id="54" presence="required"/>
        <fixr:componentRef id="1003"/>
        <fixr:componentRef id="1025" presence="required"/>
      </fixr:structure>
      <fixr:responses><fixr:response name="Ack"><fixr:messageRef name="ExecutionReport" msgType="8"/></fixr:response></fixr:responses>
    </fixr:message>
    <fixr:message id="14" name="NewOrderCross" msgType="D" scenario="Cross" category="CrossOrders">
      <fixr:structure><fixr:fieldRef id="11" presence="required" scenario="Cross"/></fixr:structure>
    </fixr:message>
  </fixr:messages>
</fixr:repository>`

// testOrchestraQuickFIX is the QuickFIX dictionary testOrchestraXML
// translates into.
const testOrchestraQuickFIX = `<fix type="FIX" major="4" minor="4">
  <header>
    <field name="BeginString" required="Y"/>
    <field name="BodyLength" required="Y"/>
    <field name="MsgType" required="Y"/>
    <field name="SenderCompID" required="Y"/>
    <field name="TargetCompID" required="Y"/>
    <field name="MsgSeqNum" required="Y"/>
    <field name="SendingTime" required="Y"/>
  </header>
  <trailer><field name="CheckSum" required="Y"/></trailer>
  <messages>
    <message name="Heartbeat" msgtype="0" msgcat="admin"><field name="TestReqID" required="N"/></message>
    <message name="NewOrderSingle" msgtype="D" msgcat="app">
      <field name="ClOrdID" required="Y"/>
      <group name="NoPartyIDs" required="N">
        <field name="PartyID" required="Y"/>
        <field name="PartyRole" required="N"/>
        <group name="NoPartySubIDs" required="N"><field name="PartySubID" required="N"/></group>
      </group>
      <field name="Side" required="Y"/>
      <component name="Instrument" required="N"/>
    </message>
  </messages>
  <components>
    <component name="Instrument">
      <field name="Symbol" required="Y"/>
      <field name="SecurityID" required="N"/>
      <field name="SecurityIDSource" required="N"/>
    </component>
  </components>
  <fields>
    <field number="8" name="BeginString" type="STRING"/>
    <field number="9" name="BodyLength" type="LENGTH"/>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="49" name="SenderCompID" type="STRING"/>
    <field number="56" name="TargetCompID" type="STRING"/>
    <field number="34" name="MsgSeqNum" type="SEQNUM"/>
    <field number="52" name="SendingTime" type="UTCTIMESTAMP"/>
    <field number="10" name="CheckSum" type="STRING"/>
    <field number="112" name="TestReqID" type="STRING"/>
    <field number="11" name="ClOrdID" type="STRING"/>
    <field number="54" name="Side" type="CHAR"><value enum="1" description="BUY"/><value enum="5" description="SELL_SHORT"/></field>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="48" name="SecurityID" type="STRING"/>
    <field number="22" name="SecurityIDSource" type="STRING"/>
    <field number="221" name="BenchmarkCurveName" type="STRING">
      <value enum="Euribor" description="EURIBOR_DEPRECATED"/><value enum="EURIBOR" description="EURIBOR"/>
    </field>
    <field number="453" name="NoPartyIDs" type="NUMINGROUP"/>
    <field number="448" name="PartyID" type="STRING"/>
    <field number="452" name="PartyRole" type="INT"/>
    <field number="802" name="NoPartySubIDs" type="NUMINGROUP"/>
    <field number="523" name="PartySubID" type="STRING"/>
  </fields>
</fix>`

func TestLoadOrchestra(t *testing.T) {
	got, err := Load(strings.NewReader(testOrchestraXML))
	if err != nil {
		t.Fatal(err)
	}
	want, err := Load(strings.NewReader(testOrchestraQuickFIX))
	if err != nil {
		t.Fatal(err)
	}
	if diff := dicttest.Diff(got, want); diff != "" {
		t.Error(diff)
	}
	if c := got.Messages["D"].Conditions; len(c) != 1 || c[0].Component != "Instrument" {
		t.Errorf("NewOrderSingle conditions = %+v, want the Instrument component's", c)
	}
}

// TestLoadOrchestraFile loads the trimmed FIX Latest repository fixgen tests
// with, read whole and byte by byte.
func TestLoadOrchestraFile(t *testing.T) {
	data, err := os.ReadFile("../../cmd/fixgen/testdata/OrchestraFIXLatest.xml")
	if err != nil {
		t.Fatal(err)
	}
	whole, err := decode(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := decode(iotest.OneByteReader(strings.NewReader(string(data))))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(whole, bytes) {
		t.Error("decoding byte by byte differs from decoding whole")
	}
	d, err := Load(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != "FIX" || d.Major != "Latest" || d.Minor != "" || d.ServicePack != "" {
		t.Errorf("version = %q %q %q %q, want FIX Latest", d.Type, d.Major, d.Minor, d.ServicePack)
	}
	if len(d.HeaderGroups) != 1 || d.HeaderGroups[0].Name != "NoHops" {
		t.Errorf("header groups = %+v, want NoHops", d.HeaderGroups)
	}
	for mt, cat := range map[string]string{"A": "admin", "n": "admin", "j": "app", "CX": "app"} {
		if md := d.Messages[mt]; md == nil || md.MsgCat != cat {
			t.Errorf("message %s = %+v, want category %s", mt, md, cat)
		}
	}
	if v := d.FieldByName("ApplVerID").Enums["10"]; v != "FIX_LATEST" {
		t.Errorf("ApplVerID 10 = %q, want FIX_LATEST", v)
	}
}

func TestLoadOrchestraErrors(t *testing.T) {
	repo := func(name, version, body string) string {
		return fmt.Sprintf(`<fixr:repository xmlns:fixr="%s" name="%s" version="%s">%s</fixr:repository>`, orchestraNS, name, version, body)
	}
	fields := `<fixr:fields><fixr:field id="1" name="A" type="String"/><fixr:field id="2" name="NoG" type="NumInGroup"/></fixr:fields>`
	message := func(structure string) string {
		return fields + `<fixr:messages><fixr:message name="M" msgType="M"><fixr:structure>` + structure + `</fixr:structure></fixr:message></fixr:messages>`
	}
	for name, c := range map[string]struct{ xml, want string }{
		"unknown field id":     {repo("FIX.4.4", "", message(`<fixr:fieldRef id="9"/>`)), "message M: unknown field id 9"},
		"unknown component id": {repo("FIX.4.4", "", message(`<fixr:componentRef id="9"/>`)), "unknown component id 9"},
		"unknown group id":     {repo("FIX.4.4", "", message(`<fixr:groupRef id="9"/>`)), "unknown group id 9"},
		"no NumInGroup": {repo("FIX.4.4", "", message(`<fixr:groupRef id="5"/>`)+
			`<fixr:groups><fixr:group id="5" name="G"><fixr:fieldRef id="1"/></fixr:group></fixr:groups>`), "group G: no numInGroup"},
		"unknown NumInGroup": {repo("FIX.4.4", "", message(`<fixr:groupRef id="5"/>`)+
			`<fixr:groups><fixr:group id="5" name="G"><fixr:numInGroup id="9"/><fixr:fieldRef id="1"/></fixr:group></fixr:groups>`), "group G: unknown NumInGroup field id 9"},
		"bad lengthId": {repo("FIX.4.4", "", `<fixr:fields><fixr:field id="1" name="A" type="data" lengthId="x"/></fixr:fields>`), `field "A": lengthId:`},
		"empty group": {repo("FIX.4.4", "", message(`<fixr:groupRef id="5"/>`)+
			`<fixr:groups><fixr:group id="5" name="G"><fixr:numInGroup id="2"/></fixr:group></fixr:groups>`), "group NoG: no child fields"},
		"group cycle": {repo("FIX.4.4", "", message(`<fixr:groupRef id="5"/>`)+
			`<fixr:groups><fixr:group id="5" name="G"><fixr:numInGroup id="2"/><fixr:fieldRef id="1"/><fixr:groupRef id="5"/></fixr:group></fixr:groups>`), `cycle detected in group "G"`},
		"component cycle": {repo("FIX.4.4", "", message(`<fixr:componentRef id="5"/>`)+
			`<fixr:components><fixr:component id="5" name="C"><fixr:componentRef id="5"/></fixr:component></fixr:components>`), `cycle detected in component "C"`},
		"same component id": {repo("FIX.4.4", "", fields+
			`<fixr:components><fixr:component id="5" name="C"/><fixr:component id="5" name="D"/></fixr:components>`), "components C and D have the same id 5"},
		"same component name": {repo("FIX.4.4", "", fields+
			`<fixr:components><fixr:component id="5" name="C"/><fixr:component id="6" name="C"/></fixr:components>`), "components 5 and 6 have the same name C"},
		"same group id": {repo("FIX.4.4", "", fields+
			`<fixr:groups><fixr:group id="5" name="G"/><fixr:group id="5" name="H"/></fixr:groups>`), "groups G and H have the same id 5"},
		"same field id": {repo("FIX.4.4", "", `<fixr:fields><fixr:field id="1" name="A" type="String"/><fixr:field id="1" name="B" type="String"/></fixr:fields>`),
			"fields A and B have the same number 1"},
		"same MsgType": {repo("FIX.4.4", "", fields+`<fixr:messages><fixr:message name="M" msgType="M"/><fixr:message name="N" msgType="M"/></fixr:messages>`),
			`messages M and N have the same MsgType "M"`},
		"field without id":     {repo("FIX.4.4", "", `<fixr:fields><fixr:field name="A" type="String"/></fixr:fields>`), `field "A": no id`},
		"reference without id": {repo("FIX.4.4", "", message(`<fixr:fieldRef/>`)), "fieldRef: no id"},
		"bad id":               {repo("FIX.4.4", "", `<fixr:fields><fixr:field id="x" name="A" type="String"/></fixr:fields>`), `field "A": id:`},
		"other scenario":       {repo("FIX.4.4", "", message(`<fixr:fieldRef id="1" scenario="Other"/>`)), `fieldRef 1: scenario "Other" is not supported`},
		"no version":           {repo("Venue", "2.0", ""), `repository "Venue": no FIX version in version "2.0"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(strings.NewReader(c.xml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Load = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// TestLoadOrchestraScenarios checks that definitions of scenarios other than
// the base one are ignored, and the base one used whether named or not.
func TestLoadOrchestraScenarios(t *testing.T) {
	xml := fmt.Sprintf(`<fixr:repository xmlns:fixr="%s" name="FIX.4.4">
  <fixr:fields><fixr:field id="1" name="A" type="String"/><fixr:field id="1" name="A2" type="int" scenario="Other"/><fixr:field id="2" name="B" type="String"/></fixr:fields>
  <fixr:components>
    <fixr:component id="5" name="C" scenario="base"><fixr:fieldRef id="1" presence="required"/></fixr:component>
    <fixr:component id="5" name="C" scenario="Other"><fixr:fieldRef id="2"/></fixr:component>
  </fixr:components>
  <fixr:messages>
    <fixr:message name="M" msgType="M"><fixr:structure><fixr:componentRef id="5" scenario="base" presence="required"/></fixr:structure></fixr:message>
    <fixr:message name="M" msgType="M" scenario="Other"><fixr:structure><fixr:fieldRef id="2"/></fixr:structure></fixr:message>
  </fixr:messages>
</fixr:repository>`, orchestraNS)
	d, err := Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	if f := d.Fields[1]; f.Name != "A" || f.Type != "STRING" {
		t.Errorf("field 1 = %+v, want the base A", f)
	}
	if got := fmt.Sprint(d.Messages["M"].Fields); got != "[{1 true}]" {
		t.Errorf("M fields = %s, want the base component's [{1 true}]", got)
	}
}

func TestLoadOrchestraLimits(t *testing.T) {
	// Skipped elements count, up to the larger Orchestra limit.
	repo := func(elements int) string {
		return fmt.Sprintf(`<fixr:repository xmlns:fixr="%s" name="FIX.4.4">`, orchestraNS) + strings.Repeat("<x/>", elements-1) + `</fixr:repository>`
	}
	if _, err := Load(strings.NewReader(repo(maxOrchestraElements))); err != nil {
		t.Errorf("maxOrchestraElements elements: %v", err)
	}
	if _, err := Load(strings.NewReader(repo(maxOrchestraElements + 1))); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("maxOrchestraElements+1 elements: got %v, want element limit error", err)
	}

	// Each group references the previous one twice: 2^n items.
	var fields, groups strings.Builder
	fields.WriteString(`<fixr:field id="1" name="F" type="String"/>`)
	groups.WriteString(`<fixr:group id="100" name="G0"><fixr:numInGroup id="200"/><fixr:fieldRef id="1"/></fixr:group>`)
	fmt.Fprintf(&fields, `<fixr:field id="200" name="N0" type="NumInGroup"/>`)
	const levels = 30
	for i := 1; i <= levels; i++ {
		fmt.Fprintf(&fields, `<fixr:field id="%d" name="N%d" type="NumInGroup"/>`, 200+i, i)
		fmt.Fprintf(&groups, `<fixr:group id="%d" name="G%d"><fixr:numInGroup id="%d"/><fixr:fieldRef id="1"/><fixr:groupRef id="%d"/><fixr:groupRef id="%d"/></fixr:group>`, 100+i, i, 200+i, 99+i, 99+i)
	}
	xml := fmt.Sprintf(`<fixr:repository xmlns:fixr="%s" name="FIX.4.4"><fixr:fields>%s</fixr:fields><fixr:groups>%s</fixr:groups>
<fixr:messages><fixr:message name="M" msgType="M"><fixr:structure><fixr:groupRef id="%d"/></fixr:structure></fixr:message></fixr:messages></fixr:repository>`,
		orchestraNS, fields.String(), groups.String(), 100+levels)
	if err := loadWithin(t, xml); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("expansion exceeds %d", maxOrchestraVisits)) {
		t.Fatalf("expected expansion error, got %v", err)
	}
}

// TestLoadOrchestraEnumLimit checks that the enum values code sets give
// fields are bounded: one code set can type any number of fields.
func TestLoadOrchestraEnumLimit(t *testing.T) {
	repo := func(codes, fields int) string {
		var b strings.Builder
		fmt.Fprintf(&b, `<fixr:repository xmlns:fixr="%s" name="FIX.4.4"><fixr:codeSets><fixr:codeSet name="CS" type="String">`, orchestraNS)
		for i := range codes {
			fmt.Fprintf(&b, `<fixr:code name="C%d" value="%d"/>`, i, i)
		}
		b.WriteString(`</fixr:codeSet></fixr:codeSets><fixr:fields>`)
		for i := range fields {
			fmt.Fprintf(&b, `<fixr:field id="%d" name="F%d" type="CS"/>`, i+1, i)
		}
		b.WriteString(`</fixr:fields></fixr:repository>`)
		return b.String()
	}
	if _, err := Load(strings.NewReader(repo(1024, maxOrchestraEnumValues/1024))); err != nil {
		t.Errorf("maxOrchestraEnumValues values: %v", err)
	}
	err := loadWithin(t, repo(1024, maxOrchestraEnumValues/1024+1))
	if err == nil || !strings.Contains(err.Error(), "enum values") {
		t.Errorf("maxOrchestraEnumValues+1024 values: got %v, want enum values limit error", err)
	}
}

// TestLoadOrchestraDataLength checks that the length field a data field
// names is a LENGTH, which pairs them for parsing, as FIX 4.2 types it int.
func TestLoadOrchestraDataLength(t *testing.T) {
	xml := fmt.Sprintf(`<fixr:repository xmlns:fixr="%s" name="FIX.4.2">
  <fixr:fields>
    <fixr:field id="95" name="RawDataLength" type="int"/>
    <fixr:field id="96" name="RawData" type="data" lengthId="95"/>
    <fixr:field id="212" name="XmlDataLen" type="Length"/>
    <fixr:field id="213" name="XmlData" type="XMLData" lengthId="212"/>
    <fixr:field id="1" name="Account" type="String"/>
    <fixr:field id="2" name="Odd" type="data" lengthId="1"/>
  </fixr:fields>
</fixr:repository>`, orchestraNS)
	d, err := Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	for tag, typ := range map[int]string{95: "LENGTH", 212: "LENGTH", 1: "STRING"} {
		if got := d.Fields[tag].Type; got != typ {
			t.Errorf("field %d type = %s, want %s", tag, got, typ)
		}
	}
	for data, length := range map[int]int{96: 95, 213: 212} {
		if got := d.Profile().DataLengthTag(data); got != length {
			t.Errorf("length of data field %d = %d, want %d", data, got, length)
		}
	}
}

// TestLoadOrchestraNamespace checks that a repository in another Orchestra
// namespace is rejected rather than read as an empty QuickFIX dictionary.
func TestLoadOrchestraNamespace(t *testing.T) {
	xml := `<fixr:repository xmlns:fixr="http://fixprotocol.io/2016/fixrepository" name="FIX.4.4"><fixr:fields/></fixr:repository>`
	if _, err := Load(strings.NewReader(xml)); err == nil || !strings.Contains(err.Error(), "is not supported") {
		t.Errorf("Load = %v, want unsupported namespace", err)
	}
}

// TestQuickFIXGroupRef checks that a QuickFIX dictionary cannot reference a
// group by name: only Orchestra repositories define groups apart.
func TestQuickFIXGroupRef(t *testing.T) {
	xml := `<fix><messages><message name="M" msgtype="M"><groupRef name="G"/></message></messages></fix>`
	if _, err := Load(strings.NewReader(xml)); err == nil || !strings.Contains(err.Error(), `unknown group "G"`) {
		t.Errorf("Load = %v, want unknown group", err)
	}
}

func TestOrchestraVersion(t *testing.T) {
	for _, c := range []struct {
		name, version string
		want          string // type major minor servicepack, or "" if none
	}{
		{"FIX.4.4", "FIX.4.4_EP311", "FIX 4 4 "},
		{"FIX.4.2", "", "FIX 4 2 "},
		{"FIX4SESSION", "FIX.4.4", "FIX 4 4 "},
		{"FIXT", "FIX.5.0SP2_EP247", "FIXT 1 1 "},
		{"FIXT.1.1", "", "FIXT 1 1 "},
		{"FIX.Latest", "FIX.Latest_EP312", "FIX Latest  "},
		{"FIX Latest for Equities", "FIX.Latest_EP272", "FIX Latest  "},
		{"Venue", "FIX.5.0SP2_EP254", "FIX 5 0 2"},
		{"Venue", "FIX.5.0", "FIX 5 0 "},
		{"Venue", "2.0", ""},
		{"", "FIX.4", ""},
		{"", "FIX.4.4SP", ""},
		{"", "FIX.a.b", ""},
		{"", "FIXML.4.4", ""},
		{"", "", ""},
	} {
		typ, major, minor, sp, ok := orchestraVersion(c.name, c.version)
		got := ""
		if ok {
			got = typ + " " + major + " " + minor + " " + sp
		}
		if got != c.want {
			t.Errorf("orchestraVersion(%q, %q) = %q, want %q", c.name, c.version, got, c.want)
		}
	}
}

func TestEnumDescription(t *testing.T) {
	for name, want := range map[string]string{
		"New":               "NEW",
		"PartiallyFilled":   "PARTIALLY_FILLED",
		"ISINNumber":        "ISIN_NUMBER",
		"GTC":               "GTC",
		"FIX50SP2":          "FIX50_SP2",
		"FIXLatest":         "FIX_LATEST",
		"PercentageWaived4": "PERCENTAGE_WAIVED4",
		"Under_Score":       "UNDER_SCORE",
		"":                  "",
	} {
		if got := enumDescription(name); got != want {
			t.Errorf("enumDescription(%q) = %q, want %q", name, got, want)
		}
	}
}
