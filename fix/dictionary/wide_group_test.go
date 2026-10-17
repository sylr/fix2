package dictionary_test

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
)

// wideGroupDict returns a dictionary whose message Z has a group NoG (73) of
// width tags: half of them optional fields, 1000 on, then an optional
// component C of the other half, whose first is required.
func wideGroupDict(tb testing.TB, width int) *dictionary.Dict {
	tb.Helper()
	var x strings.Builder
	x.WriteString(`<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/>`)
	x.WriteString(`<messages><message name="M" msgtype="Z"><group name="NoG" required="Y">`)
	for i := range width / 2 {
		fmt.Fprintf(&x, `<field name="F%d" required="N"/>`, 1000+i)
	}
	x.WriteString(`<component name="C" required="N"/></group></message></messages><components><component name="C">`)
	for i := width / 2; i < width; i++ {
		required := "N"
		if i == width/2 {
			required = "Y"
		}
		fmt.Fprintf(&x, `<field name="F%d" required="%s"/>`, 1000+i, required)
	}
	x.WriteString(`</component></components><fields><field number="35" name="MsgType" type="STRING"/><field number="73" name="NoG" type="NUMINGROUP"/>`)
	for i := range width {
		fmt.Fprintf(&x, `<field number="%d" name="F%d" type="STRING"/>`, 1000+i, 1000+i)
	}
	x.WriteString(`</fields></fix>`)
	d, err := xmldict.Load(strings.NewReader(x.String()))
	if err != nil {
		tb.Fatal(err)
	}
	return d
}

// wideGroupMsg returns a message Z of d, of a group as wideGroupDict's of
// width, with entries entries holding the group's first and last fields, and
// none of its component's.
func wideGroupMsg(tb testing.TB, d *dictionary.Dict, width, entries int) *fix.Message {
	tb.Helper()
	b := fix.NewBuilder("FIX.4.4", "Z").AppendInt(73, entries)
	for range entries {
		b.AppendString(1000, "x").AppendString(1000+width/2-1, "y")
	}
	m := new(fix.Message)
	if err := fix.ParseInto(m, b.Build(), d.Profile()); err != nil {
		tb.Fatal(err)
	}
	return m
}

// A group too wide for a bitset on the stack, whose tags are looked up in an
// index, is checked as a narrow one: an entry's tags do not outlive it, nor
// a validation's, and an optional component it does not hold costs nothing
// and requires nothing.
func TestValidateWith_WideGroupEntries(t *testing.T) {
	const width = 600 // 10 words of bitset
	d := wideGroupDict(t, width)
	for _, tc := range []struct {
		name   string
		body   string
		tag    int // 0 for valid
		reason dictionary.RejectReason
		msg    string // in the error text
	}{
		{name: "valid", body: "73=2|1000=a|1299=b|1000=c|1299=d|"},
		{name: "component complete", body: "73=2|1000=a|1300=b|1599=c|1000=d|1100=e|"},
		{"component without its required field", "73=2|1000=a|1100=b|1000=c|1599=d|", 1300, dictionary.ReasonRequiredTagMissing,
			"required tag 1300 (F1300) missing for component C in entry 2 of group 73 (NoG) in message M"},
		// After an error mid-entry, the scratch is clean for the next.
		{"field out of order", "73=1|1000=a|1299=b|1100=c|", 1100, dictionary.ReasonRepeatingGroupOutOfOrder,
			"tag 1100 (F1100) out of order in entry 1 of group 73 (NoG) in message M"},
		{name: "valid after an error", body: "73=1|1000=a|1100=b|1299=c|"},
		{"repeated tag ends the entry", "73=1|1000=a|1100=b|1100=c|", 1100, dictionary.ReasonTagNotDefinedForMessageType,
			"tag 1100 (F1100) not allowed outside group entries in message M"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parseMsg(t, buildFIXMsg("35=Z\x01"+strings.ReplaceAll(tc.body, "|", "\x01")))
			err := d.ValidateWith(&m, dictionary.CheckAll)
			if tc.tag == 0 {
				if err != nil {
					t.Errorf("CheckAll: %v, want nil", err)
				}
				return
			}
			var ve *dictionary.ValidationError
			if !errors.As(err, &ve) || ve.Tag != tc.tag || ve.Reason != tc.reason {
				t.Fatalf("CheckAll: %v, want tag %d, reason %d", err, tc.tag, tc.reason)
			}
			if !strings.Contains(ve.Msg, tc.msg) {
				t.Errorf("CheckAll: %q, want it to contain %q", ve.Msg, tc.msg)
			}
		})
	}
}

// Strict validation of small entries costs about the same whatever the
// width of their group and of an optional component they do not hold: it
// once cost entries × width, 400 times as much at 65,536 tags as at 64.
// Timings are compared rather than asserted, each the best of a few runs,
// interleaved and after a collection of the dictionaries' garbage, with a
// margin wide enough for -race and a loaded machine: a linear tag lookup or
// a scan of the component's activation tags left would cost over 160 times
// as much. Clearing the whole bitset per entry, which costs about twice as
// much at that width, is left to BenchmarkValidateWith_WideGroup.
func TestValidateWith_WideGroupCost(t *testing.T) {
	const entries = 20000
	widths := []int{64, 4096, 65536}
	dicts := make([]*dictionary.Dict, len(widths))
	msgs := make([]*fix.Message, len(widths))
	for i, width := range widths {
		dicts[i] = wideGroupDict(t, width)
		msgs[i] = wideGroupMsg(t, dicts[i], width, entries)
	}
	runtime.GC()
	best := make([]time.Duration, len(widths))
	for range 5 {
		for i := range widths {
			start := time.Now()
			if err := dicts[i].ValidateWith(msgs[i], dictionary.CheckAll); err != nil {
				t.Fatal(err)
			}
			if el := time.Since(start); best[i] == 0 || el < best[i] {
				best[i] = el
			}
		}
	}
	for i, width := range widths {
		t.Logf("width=%d entries=%d CheckAll=%v", width, entries, best[i])
	}
	if narrow, wide := best[0], best[len(best)-1]; wide > 20*narrow {
		t.Errorf("CheckAll at width %d took %v, more than 20 times %v at width %d", widths[len(widths)-1], wide, narrow, widths[0])
	}
}

// wideNestedXML is a dictionary whose message Z has a group NoG (73) of
// 351 tags: optional fields 1000 to 1299, the first required, an optional
// component C of fields 1300 to 1349, the first required, and a group NoN
// (74) of fields 2000 to 2299, the first and last required.
var wideNestedXML = func() string {
	var x strings.Builder
	field := func(tag int, required bool) {
		fmt.Fprintf(&x, `<field name="F%d" required="%s"/>`, tag, map[bool]string{false: "N", true: "Y"}[required])
	}
	x.WriteString(`<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/>`)
	x.WriteString(`<messages><message name="M" msgtype="Z"><group name="NoG" required="N">`)
	for tag := 1000; tag < 1300; tag++ {
		field(tag, tag == 1000)
	}
	x.WriteString(`<component name="C" required="N"/><group name="NoN" required="N">`)
	for tag := 2000; tag < 2300; tag++ {
		field(tag, tag == 2000 || tag == 2299)
	}
	x.WriteString(`</group></group></message></messages><components><component name="C">`)
	for tag := 1300; tag < 1350; tag++ {
		field(tag, tag == 1300)
	}
	x.WriteString(`</component></components><fields><field number="35" name="MsgType" type="STRING"/>`)
	x.WriteString(`<field number="73" name="NoG" type="NUMINGROUP"/><field number="74" name="NoN" type="NUMINGROUP"/>`)
	for _, r := range [][2]int{{1000, 1350}, {2000, 2300}} {
		for tag := r[0]; tag < r[1]; tag++ {
			fmt.Fprintf(&x, `<field number="%d" name="F%d" type="STRING"/>`, tag, tag)
		}
	}
	x.WriteString(`</fields></fix>`)
	return x.String()
}()

// A wide group nested in a wide group is checked as narrow ones are, and an
// error in a nested entry leaves the scratches of both clean for the next
// message, which holds the same tags.
func TestValidateWith_WideNestedGroups(t *testing.T) {
	d, err := xmldict.Load(strings.NewReader(wideNestedXML))
	if err != nil {
		t.Fatal(err)
	}
	const valid = "73=1|1000=a|1299=b|1300=d|1301=c|74=1|2000=e|2150=f|2299=g|"
	for _, tc := range []struct {
		name   string
		body   string
		tag    int // 0 for valid
		reason dictionary.RejectReason
		msg    string // in the error text
	}{
		{name: "valid", body: "73=2|1000=a|1299=b|74=2|2000=c|2150=d|2299=e|2000=f|2299=g|1000=h|1300=i|74=1|2000=j|2299=k|"},
		{"nested entry out of order", "73=1|1000=a|1299=b|1300=d|1301=c|74=1|2000=e|2299=g|2150=f|", 2150, dictionary.ReasonRepeatingGroupOutOfOrder,
			"tag 2150 (F2150) out of order in entry 1 of group 74 (NoN) in message M"},
		{name: "valid after an order error", body: valid},
		{"nested entry missing a field", "73=1|1000=a|1299=b|1300=d|1301=c|74=1|2000=e|2150=f|", 2299, dictionary.ReasonRequiredTagMissing,
			"required tag 2299 (F2299) missing in entry 1 of group 74 (NoN) in message M"},
		{name: "valid after a missing field", body: valid},
		{"component missing a field", "73=1|1000=a|1299=b|1301=c|74=1|2000=e|2299=g|", 1300, dictionary.ReasonRequiredTagMissing,
			"required tag 1300 (F1300) missing for component C in entry 1 of group 73 (NoG)"},
		{name: "valid after a component's missing field", body: valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parseMsg(t, buildFIXMsg("35=Z\x01"+strings.ReplaceAll(tc.body, "|", "\x01")))
			err := d.ValidateWith(&m, dictionary.CheckAll)
			if tc.tag == 0 {
				if err != nil {
					t.Errorf("CheckAll: %v, want nil", err)
				}
				return
			}
			var ve *dictionary.ValidationError
			if !errors.As(err, &ve) || ve.Tag != tc.tag || ve.Reason != tc.reason || !strings.Contains(ve.Msg, tc.msg) {
				t.Fatalf("CheckAll: %v, want tag %d, reason %d, %q", err, tc.tag, tc.reason, tc.msg)
			}
		})
	}
}

// A nested group an optional component declares again, counter and all,
// is the entry's first: its rules, its required counter's included, are
// those of the copy the walk finds, not of the last.
func TestValidateWith_NestedGroupInComponentToo(t *testing.T) {
	const x = `<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/>
<messages><message name="M" msgtype="Z"><group name="NoG" required="Y">
  <field name="F1000" required="Y"/>
  <group name="NoN" required="Y"><field name="F2000" required="Y"/></group>
  <component name="C" required="N"/>
</group></message></messages>
<components><component name="C"><group name="NoN" required="N"><field name="F2000" required="Y"/></group></component></components>
<fields><field number="35" name="MsgType" type="STRING"/><field number="73" name="NoG" type="NUMINGROUP"/>
<field number="74" name="NoN" type="NUMINGROUP"/><field number="1000" name="F1000" type="STRING"/><field number="2000" name="F2000" type="STRING"/></fields></fix>`
	d, err := xmldict.Load(strings.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	m := parseMsg(t, buildFIXMsg("35=Z\x0173=1\x011000=a\x0174=1\x012000=b\x01"))
	if err := d.ValidateWith(&m, dictionary.CheckAll); err != nil {
		t.Errorf("CheckAll: %v, want nil", err)
	}
	// Required, the nested group needs an entry.
	m = parseMsg(t, buildFIXMsg("35=Z\x0173=1\x011000=a\x0174=0\x01"))
	var ve *dictionary.ValidationError
	if err := d.ValidateWith(&m, dictionary.CheckAll); !errors.As(err, &ve) || ve.Tag != 74 || ve.Reason != dictionary.ReasonIncorrectNumInGroupCount {
		t.Errorf("CheckAll with NoN = 0: %v, want an incorrect NumInGroup for 74", err)
	}
	m = parseMsg(t, buildFIXMsg("35=Z\x011000=x\x01"))
	if err := d.ValidateWith(&m, dictionary.CheckAll); !errors.As(err, &ve) || ve.Tag != 73 {
		t.Errorf("CheckAll without NoG: %v, want tag 73 missing", err)
	}
	m = parseMsg(t, buildFIXMsg("35=Z\x0173=1\x011000=a\x01"))
	if err := d.ValidateWith(&m, dictionary.CheckAll); !errors.As(err, &ve) || ve.Tag != 74 {
		t.Errorf("CheckAll without NoN: %v, want tag 74 missing", err)
	}
}

// An entry activating more optional components than the walk keeps distinct
// on the stack reports the first, in the group's order, missing a field.
func TestValidateWith_GroupEntryManyComponents(t *testing.T) {
	const n = 20
	var x strings.Builder
	x.WriteString(`<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/>`)
	x.WriteString(`<messages><message name="M" msgtype="Z"><group name="NoG" required="Y"><field name="F1000" required="Y"/>`)
	for c := range n {
		fmt.Fprintf(&x, `<component name="C%d" required="N"/>`, c)
	}
	x.WriteString(`</group></message></messages><components>`)
	for c := range n {
		fmt.Fprintf(&x, `<component name="C%d"><field name="F%d" required="Y"/><field name="F%d" required="N"/></component>`, c, 1001+2*c, 1002+2*c)
	}
	x.WriteString(`</components><fields><field number="35" name="MsgType" type="STRING"/><field number="73" name="NoG" type="NUMINGROUP"/>`)
	for i := range 1 + 2*n {
		fmt.Fprintf(&x, `<field number="%d" name="F%d" type="STRING"/>`, 1000+i, 1000+i)
	}
	x.WriteString(`</fields></fix>`)
	d, err := xmldict.Load(strings.NewReader(x.String()))
	if err != nil {
		t.Fatal(err)
	}
	// Every component but C3 complete, then all of them without their
	// required fields but C3 and C7.
	var complete, partial strings.Builder
	for c := range n {
		if c != 3 {
			fmt.Fprintf(&complete, "%d=r|", 1001+2*c)
		}
		if c == 3 || c == 7 {
			fmt.Fprintf(&partial, "%d=r|", 1001+2*c)
		}
		fmt.Fprintf(&complete, "%d=o|", 1002+2*c)
		fmt.Fprintf(&partial, "%d=o|", 1002+2*c)
	}
	for _, tc := range []struct {
		name, body string
		tag        int
		msg        string
	}{
		{"one missing", "73=1|1000=a|" + complete.String(), 1007, "missing for component C3 in entry 1"},
		{"first missing reported", "73=1|1000=a|" + partial.String(), 1001, "missing for component C0 in entry 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := parseMsg(t, buildFIXMsg("35=Z\x01"+strings.ReplaceAll(tc.body, "|", "\x01")))
			var ve *dictionary.ValidationError
			if err := d.ValidateWith(&m, dictionary.CheckAll); !errors.As(err, &ve) || ve.Tag != tc.tag || !strings.Contains(ve.Msg, tc.msg) {
				t.Fatalf("CheckAll: %v, want tag %d, %q", err, tc.tag, tc.msg)
			}
		})
	}
}
