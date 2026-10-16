package xmldict

import (
	"encoding/xml"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
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
	d, err := Load(strings.NewReader(testDictXML))
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

func TestResolve_ComponentFlattened(t *testing.T) {
	d := loadTestDict(t)
	nos := d.Messages["D"]

	// Symbol (tag 55) should be in the flattened field list via Instrument component.
	if !hasField(nos, 55) {
		t.Fatal("Symbol (55) should be in NewOrderSingle fields via Instrument component")
	}
}

func TestResolve_GroupCounterAndChildren(t *testing.T) {
	d := loadTestDict(t)
	nos := d.Messages["D"]

	// NoAllocs counter (tag 78) should be present.
	if !hasField(nos, 78) {
		t.Fatal("NoAllocs counter (78) should be in NewOrderSingle fields")
	}
	// AllocAccount (tag 79) should be present (group child).
	if !hasField(nos, 79) {
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

func TestResolve_NestedGroupsAndChildOrder(t *testing.T) {
	d, err := Load(strings.NewReader(nestedDictXML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	md := d.Messages["AB"]

	tags := func(refs []dictionary.FieldRef) []int {
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

func TestLoad_ExponentialExpansion(t *testing.T) {
	for name, c0 := range map[string]string{
		"leaf field": `<component name="C0"><field name="F" required="N"/></component>`,
		"empty leaf": `<component name="C0"/>`,
	} {
		t.Run(name, func(t *testing.T) { testExponentialExpansion(t, c0) })
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
		byName:    map[string]*dictionary.FieldDef{"NoG": {Tag: 100}, "X": {Tag: 101}, "Y": {Tag: 102}},
		compMap:   map[string]*xmlComponent{},
		visiting:  map[visit]bool{},
		maxVisits: maxVisits,
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
	d, err := LoadFile("../testdata/FIXT11.xml")
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

// endless yields 'a' forever.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// hasField reports whether m's flattened fields include tag.
func hasField(m *dictionary.MessageDef, tag int) bool {
	return slices.ContainsFunc(m.Fields, func(r dictionary.FieldRef) bool { return r.Tag == tag })
}
