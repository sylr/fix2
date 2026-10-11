package dictionary_test

import (
	"strings"
	"testing"

	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
)

func TestDescribe(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(`<fix type="FIXT" major="1" minor="1">
  <header>
    <field name="BeginString" required="Y"/>
    <field name="MsgType" required="Y"/>
    <group name="NoHops" required="N"><field name="HopCompID" required="N"/></group>
  </header>
  <trailer><field name="CheckSum" required="Y"/></trailer>
  <messages><message name="Order" msgtype="D" msgcat="app"><field name="Side" required="N"/></message></messages>
  <components/>
  <fields>
    <field number="8" name="BeginString" type="STRING"/>
    <field number="35" name="MsgType" type="STRING"><value enum="D" description="NEW_ORDER_SINGLE"/></field>
    <field number="627" name="NoHops" type="NUMINGROUP"/>
    <field number="628" name="HopCompID" type="STRING"/>
    <field number="10" name="CheckSum" type="STRING"/>
    <field number="54" name="Side" type="CHAR"><value enum="1" description="BUY"/></field>
  </fields>
</fix>`))
	if err != nil {
		t.Fatal(err)
	}
	for tag, want := range map[int]dictionary.Section{
		8: dictionary.SectionHeader, 35: dictionary.SectionHeader, 627: dictionary.SectionHeader, 628: dictionary.SectionHeader,
		10: dictionary.SectionTrailer, 54: dictionary.SectionBody, 9999: dictionary.SectionBody,
	} {
		if got := d.SectionOf(tag); got != want {
			t.Errorf("SectionOf(%d) = %s, want %s", tag, got, want)
		}
	}
	for _, c := range []struct {
		tag   int
		value string
		want  string
	}{
		{35, "D", "35(MsgType)=D(NEW_ORDER_SINGLE)"},
		{54, "7", "54(Side)=7"},
		{8, "FIXT.1.1", "8(BeginString)=FIXT.1.1"},
		{9999, "x", "9999=x"},
	} {
		if got := string(d.AppendField(nil, c.tag, []byte(c.value))); got != c.want {
			t.Errorf("AppendField(%d, %q) = %s, want %s", c.tag, c.value, got, c.want)
		}
	}
}
