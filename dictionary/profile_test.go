package dictionary_test

import (
	"strconv"
	"strings"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary/xmldict"
)

func TestDictProfile(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <field name="CustomLen" required="N"/>
      <field name="CustomBlob" required="N"/>
      <field name="EncodedUnderlyingMarketDisruptionFallbackUnderlierSecurityDesc" required="N"/>
      <field name="Orphan" required="N"/>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="95" name="RawDataLength" type="LENGTH"/>
    <field number="96" name="RawData" type="DATA"/>
    <field number="41874" name="EncodedUnderlyingMarketDisruptionFallbackUnderlierSecurityDesc" type="DATA"/>
    <field number="50001" name="CustomLen" type="LENGTH"/>
    <field number="50002" name="CustomBlob" type="DATA"/>
    <field number="50003" name="Orphan" type="DATA"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	p := d.Profile()
	for _, tc := range []struct {
		data, length int
		why          string
	}{
		{96, 95, "by name"},
		{41874, 41873, "by the standard table, its LENGTH field undeclared"},
		{50002, 50001, "by position"},
		{50003, 0, "no LENGTH field"},
	} {
		if got := p.DataLengthTag(tc.data); got != tc.length {
			t.Errorf("%s: LENGTH of %d = %d, want %d", tc.why, tc.data, got, tc.length)
		}
	}
	// Messages parse as the dictionary defines them.
	body := "35=M\x0150001=3\x0150002=a\x01b\x01"
	raw := fix2.AppendTrailer([]byte("8=FIX.4.4\x019="+strconv.Itoa(len(body))+"\x01"+body), 0)
	var m fix2.Message
	if err := fix2.ParseInto(&m, raw, p); err != nil {
		t.Fatalf("ParseInto with the dictionary's profile: %v", err)
	}
	if err := d.Validate(&m); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if fix2.ParseInto(&m, raw, nil) == nil {
		t.Error("the default profile parsed a custom DATA field holding SOH")
	}
}

// A LENGTH field ending a group does not pair with a DATA field after the
// group: they are in different scopes.
func TestDictProfile_GroupScope(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <group name="NoX" required="N"><field name="X" required="N"/><field name="XLenField" required="N"/></group>
      <field name="Payload" required="N"/>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="50000" name="NoX" type="NUMINGROUP"/>
    <field number="50001" name="X" type="STRING"/>
    <field number="50002" name="XLenField" type="LENGTH"/>
    <field number="50003" name="Payload" type="DATA"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Profile().DataLengthTag(50003); got != 0 {
		t.Errorf("DATA 50003 after a group paired with the group's LENGTH %d", got)
	}
}

// A LENGTH field a group holds can still pair with a DATA field at the top
// level, where it occurs too.
func TestDictProfile_TopLevelPairBesideGroup(t *testing.T) {
	const xml = `<fix type="FIX" major="4" minor="4">
  <header><field name="MsgType" required="Y"/></header>
  <trailer/>
  <messages>
    <message name="M" msgtype="M" msgcat="app">
      <group name="NoX" required="N"><field name="X" required="N"/><field name="Size" required="N"/></group>
      <field name="Size" required="N"/>
      <field name="Payload" required="N"/>
    </message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"/>
    <field number="50000" name="NoX" type="NUMINGROUP"/>
    <field number="50001" name="X" type="STRING"/>
    <field number="50002" name="Size" type="LENGTH"/>
    <field number="50003" name="Payload" type="DATA"/>
  </fields>
</fix>`
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Profile().DataLengthTag(50003); got != 50002 {
		t.Errorf("LENGTH of 50003 = %d, want 50002", got)
	}
}
