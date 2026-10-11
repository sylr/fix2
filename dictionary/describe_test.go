package dictionary_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
)

// describeXML declares neither BeginString, BodyLength nor CheckSum, and
// HopRefID both in the header and in message U's body.
const describeXML = `<fix type="FIX" major="4" minor="4">
  <header>
    <field name="MsgType" required="Y"/>
    <field name="HopRefID" required="N"/>
    <group name="NoHops" required="N"><field name="HopCompID" required="N"/></group>
  </header>
  <trailer/>
  <messages>
    <message name="Order" msgtype="D" msgcat="app"><field name="Side" required="N"/></message>
    <message name="Hop" msgtype="U" msgcat="app"><field name="HopRefID" required="N"/></message>
  </messages>
  <components/>
  <fields>
    <field number="35" name="MsgType" type="STRING"><value enum="D" description="NEW_ORDER_SINGLE"/></field>
    <field number="627" name="NoHops" type="NUMINGROUP"/>
    <field number="628" name="HopCompID" type="STRING"/>
    <field number="630" name="HopRefID" type="SEQNUM"/>
    <field number="54" name="Side" type="CHAR"><value enum="1" description="BUY"/></field>
  </fields>
</fix>`

func TestSections(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(describeXML))
	if err != nil {
		t.Fatal(err)
	}
	const (
		H = dictionary.SectionHeader
		B = dictionary.SectionBody
		T = dictionary.SectionTrailer
	)
	for _, c := range []struct {
		name, msg string
		want      []dictionary.Section
	}{
		{"header group", "35=D|627=2|628=A|628=B|54=1|5000=x|", []dictionary.Section{H, H, H, H, H, H, B, B, T}},
		{"tag in header and body", "35=U|630=7|", []dictionary.Section{H, H, H, B, T}},
		{"header-only tag", "35=D|630=7|54=1|", []dictionary.Section{H, H, H, H, B, T}},
		{"unknown message type", "35=ZZ|630=7|99=x|", []dictionary.Section{H, H, H, H, B, T}},
	} {
		body := strings.ReplaceAll(c.msg, "|", "\x01")
		raw := fix2.AppendTrailer([]byte("8=FIX.4.4\x019="+strconv.Itoa(len(body))+"\x01"+body), 0)
		m, err := fix2.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := d.Sections(&m); !slices.Equal(got, c.want) {
			t.Errorf("%s: Sections = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAppendField(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(describeXML))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tag   int
		value string
		want  string
	}{
		{35, "D", "35(MsgType)=D(NEW_ORDER_SINGLE)"},
		{54, "7", "54(Side)=7"},
		{9999, "x", "9999=x"},
		{58, "a\nFORGED\x1b[2J\\", `58=a\nFORGED\x1b[2J\\`},
	} {
		if got := string(d.AppendField(nil, c.tag, []byte(c.value))); got != c.want {
			t.Errorf("AppendField(%d, %q) = %s, want %s", c.tag, c.value, got, c.want)
		}
	}
}
