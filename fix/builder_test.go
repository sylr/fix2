package fix

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestBuilderRoundTrip(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").
		SetString(49, "SENDER").
		SetString(56, "TARGET").
		SetInt(34, 1).
		SetString(52, "20231015-10:00:00").
		SetString(11, "ORD-001").
		SetString(54, "1").
		SetString(55, "AAPL").
		SetInt(38, 100).
		SetFloat(44, 150.5).
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse of built message failed: %v", err)
	}

	checks := []struct {
		tag  int
		want string
	}{
		{8, "FIX.4.4"},
		{35, "D"},
		{49, "SENDER"},
		{56, "TARGET"},
		{34, "1"},
		{11, "ORD-001"},
		{54, "1"},
		{55, "AAPL"},
		{38, "100"},
		{44, "150.5"},
	}
	for _, c := range checks {
		got := string(msg.Get(c.tag))
		if got != c.want {
			t.Errorf("tag %d = %q, want %q", c.tag, got, c.want)
		}
	}
}

func TestBuilderSetReplaces(t *testing.T) {
	b := NewBuilder("FIX.4.4", "0")
	b.SetString(49, "OLD")
	b.SetString(49, "NEW")

	raw := b.Build()
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if got := string(msg.Get(49)); got != "NEW" {
		t.Errorf("SenderCompID = %q, want %q", got, "NEW")
	}
}

func TestBuilderBool(t *testing.T) {
	b := NewBuilder("FIX.4.4", "D")
	b.SetBool(114, true)  // LocateReqd
	b.SetBool(130, false) // IOINaturalFlag

	raw := b.Build()
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if got := string(msg.Get(114)); got != "Y" {
		t.Errorf("tag 114 = %q, want %q", got, "Y")
	}
	if got := string(msg.Get(130)); got != "N" {
		t.Errorf("tag 130 = %q, want %q", got, "N")
	}
}

func TestBuilderAppend(t *testing.T) {
	b := NewBuilder("FIX.4.4", "D")
	b.SetInt(78, 2)
	b.AppendString(79, "ACC1")
	b.AppendFloat(80, 500)
	b.AppendString(79, "ACC2")
	b.AppendFloat(80, 300)
	b.AppendBool(114, true)
	b.AppendInt(38, 100)

	raw := b.Build()
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Append should not replace — both tag 79 values should be present.
	var tag79Values []string
	for _, f := range msg.Fields() {
		if f.Tag == 79 {
			tag79Values = append(tag79Values, string(f.Value))
		}
	}
	if len(tag79Values) != 2 {
		t.Fatalf("expected 2 tag 79 values, got %d", len(tag79Values))
	}
	if tag79Values[0] != "ACC1" || tag79Values[1] != "ACC2" {
		t.Errorf("tag 79 values = %v, want [ACC1, ACC2]", tag79Values)
	}

	// Check AppendBool
	if got := string(msg.Get(114)); got != "Y" {
		t.Errorf("tag 114 = %q, want %q", got, "Y")
	}

	// Check AppendInt
	if got := string(msg.Get(38)); got != "100" {
		t.Errorf("tag 38 = %q, want %q", got, "100")
	}
}

func TestBuilderMinimal(t *testing.T) {
	raw := NewBuilder("FIX.4.0", "0").Build()
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if msg.Count() != 4 { // 8, 9, 35, 10
		t.Errorf("Count = %d, want 4", msg.Count())
	}
	if string(msg.MsgType()) != "0" {
		t.Errorf("MsgType = %q, want %q", msg.MsgType(), "0")
	}
}

func TestBuilderManagedTags(t *testing.T) {
	// Setting BeginString or MsgType replaces the managed value instead of
	// emitting a duplicate tag.
	raw := NewBuilder("FIX.4.4", "D").
		SetString(35, "F").
		AppendString(8, "FIX.4.2").
		SetString(11, "ORD").
		Build()
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	if got := string(msg.BeginString()); got != "FIX.4.2" {
		t.Errorf("BeginString = %q, want FIX.4.2", got)
	}
	if got := string(msg.MsgType()); got != "F" {
		t.Errorf("MsgType = %q, want F", got)
	}
	for _, tag := range []int{8, 35} {
		n := 0
		for _, f := range msg.Fields() {
			if f.Tag == tag {
				n++
			}
		}
		if n != 1 {
			t.Errorf("tag %d appears %d times in %q, want 1", tag, n, raw)
		}
	}

	// BodyLength and CheckSum are computed by Build: setting them panics.
	for _, tc := range []struct {
		name string
		call func(b *Builder)
	}{
		{"Set 9", func(b *Builder) { b.SetInt(9, 1) }},
		{"Append 9", func(b *Builder) { b.AppendInt(9, 1) }},
		{"Set 10", func(b *Builder) { b.SetString(10, "000") }},
		{"Append 10", func(b *Builder) { b.AppendString(10, "000") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected panic")
				}
			}()
			tc.call(NewBuilder("FIX.4.4", "D"))
		})
	}
}

func TestSetField(t *testing.T) {
	var fields []Field
	fields = SetField(fields, 1, []byte("a"))
	fields = SetField(fields, 2, []byte("b"))
	fields = SetField(fields, 1, []byte("c")) // replaced in place
	if len(fields) != 2 || fields[0].Tag != 1 || string(fields[0].Value) != "c" || fields[1].Tag != 2 {
		t.Errorf("SetField = %v", fields)
	}
}

func TestAppendField(t *testing.T) {
	fields := []Field{{Tag: 2, Value: []byte("b")}, {Tag: 1, Value: []byte("a")}}
	dst := AppendField(nil, fields, 1)
	dst = AppendField(dst, fields, 3) // absent: nothing appended
	dst = AppendField(dst, fields, 2)
	if len(dst) != 2 || dst[0].Tag != 1 || dst[1].Tag != 2 {
		t.Errorf("AppendField = %v", dst)
	}
}

func TestSetGroups(t *testing.T) {
	var entries []string
	b := NewBuilder("FIX.4.4", "D")
	b.SetGroups(func(dst []Field) []Field {
		if len(entries) == 0 {
			return dst
		}
		dst = AppendCount(dst, 78, len(entries))
		for _, e := range entries {
			dst = append(dst, Field{Tag: 79, Value: []byte(e)})
		}
		return dst
	}, 78)

	// A raw counter is dropped when the groups write their own.
	b.SetInt(78, 0)
	entries = []string{"A", "B"}
	// Groups are serialized whichever Build a fluent chain ends on.
	raw := b.SetString(11, "ORD").Build()
	if n := bytes.Count(raw, []byte("\x0178=")); n != 1 {
		t.Errorf("%q: %d counters, want 1", raw, n)
	}
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	if got := len(msg.Groups(78, []int{79})); got != 2 {
		t.Errorf("%q: %d entries, want 2", raw, got)
	}

	// Without group entries the raw counter is kept.
	entries = nil
	if !bytes.Contains(b.Build(), []byte("\x0178=0\x01")) {
		t.Error("raw counter dropped although the groups did not write it")
	}
}

func TestBuildWith(t *testing.T) {
	b := NewBuilder("FIX.4.4", "D").SetString(11, "ORD")
	extra := AppendCount(nil, 78, 1)
	extra = append(extra, Field{Tag: 79, Value: []byte("ACC")})
	raw := b.BuildWith(extra)
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	if got := string(msg.Get(78)); got != "1" {
		t.Errorf("78 = %q", got)
	}
	// BuildWith does not change the builder.
	if bytes.Contains(b.Build(), []byte("\x0179=")) {
		t.Error("BuildWith leaked extra fields into the builder")
	}
}

func TestBuilderSetData(t *testing.T) {
	b := NewBuilder("FIX.4.4", "A").
		SetInt(95, 99). // stale length set earlier, elsewhere in the body
		SetString(58, "t").
		SetData(95, 96, []byte("a\x01b"))
	raw := b.Build()
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	if got := string(msg.Get(96)); got != "a\x01b" {
		t.Errorf("RawData = %q", got)
	}
	if got := string(msg.Get(95)); got != "3" {
		t.Errorf("RawDataLength = %q, want 3", got)
	}
	if n := bytes.Count(raw, []byte("\x0195=")); n != 1 {
		t.Errorf("%q: RawDataLength appears %d times", raw, n)
	}
}

func TestDataLengthTag(t *testing.T) {
	for data, want := range map[int]int{96: 95, 213: 212, 1665: 1664, 89: 93, 58: 0, 1619: 0} {
		if got := DataLengthTag(data); got != want {
			t.Errorf("DataLengthTag(%d) = %d, want %d", data, got, want)
		}
	}
}

func TestDataTag(t *testing.T) {
	for length, want := range map[int]int{95: 96, 212: 213, 1664: 1665, 93: 89, 96: 0, 58: 0, -1: 0, 1 << 30: 0} {
		if got := DataTag(length); got != want {
			t.Errorf("DataTag(%d) = %d, want %d", length, got, want)
		}
	}
}

func TestBuilderSetDataManagedTags(t *testing.T) {
	for _, tags := range [][2]int{{9, 96}, {95, 35}, {8, 96}, {95, 10}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("SetData(%d, %d) did not panic", tags[0], tags[1])
				}
			}()
			NewBuilder("FIX.4.4", "A").SetData(tags[0], tags[1], []byte("x"))
		}()
	}
}

func TestSetSections(t *testing.T) {
	b := NewBuilder("FIXT.1.1", "0")
	b.SetSections([]int{49, 56, 34}, []int{93, 89}, func(dst []Field) []Field {
		return append(AppendCount(dst, 627, 1), Field{Tag: 628, Value: []byte("H")})
	})
	b.SetGroups(func(dst []Field) []Field { return dst }, 627)
	// Set in an order that mixes sections.
	b.SetString(112, "T").SetData(93, 89, []byte("sig")).SetString(49, "S").SetString(56, "D").SetInt(34, 1)
	raw := b.Build()
	var tags []int
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	for _, f := range msg.Fields() {
		tags = append(tags, f.Tag)
	}
	want := []int{8, 9, 35, 49, 56, 34, 627, 628, 112, 93, 89, 10}
	if !slices.Equal(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

// The trailer is written in the order registered: Signature set before
// SignatureLength still follows it.
func TestSetSectionsTrailerOrder(t *testing.T) {
	b := NewBuilder("FIX.4.4", "0")
	b.SetSections(nil, []int{93, 89}, nil)
	raw := b.SetString(89, "a\x01b").SetString(112, "T").SetInt(93, 3).Build()
	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	var tags []int
	for _, f := range msg.Fields() {
		tags = append(tags, f.Tag)
	}
	if want := []int{8, 9, 35, 112, 93, 89, 10}; !slices.Equal(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

func TestSetSectionsTagInBoth(t *testing.T) {
	b := NewBuilder("FIX.4.4", "0")
	b.SetSections([]int{90, 91}, []int{90, 91}, nil)
	raw := b.SetData(90, 91, []byte("x")).Build()
	if n := bytes.Count(raw, []byte("\x0191=")); n != 1 {
		t.Errorf("%q: SecureData written %d times, want 1", raw, n)
	}
}

func TestBuilder_AppendToMatchesBuild(t *testing.T) {
	b := NewBuilder("FIX.4.4", "D").
		SetString(49, "S").SetString(56, "T").SetInt(34, 7).
		SetString(11, "ORD").SetFloat(44, 1.5).AppendInt(453, 1).SetData(95, 96, []byte("a\x01b"))
	b.SetSections([]int{49, 56, 34}, []int{93}, nil)
	want := b.Build()
	prefix := []byte("prefix")
	got := b.AppendTo(prefix)
	if !bytes.Equal(got[:len(prefix)], prefix) || !bytes.Equal(got[len(prefix):], want) {
		t.Fatalf("AppendTo = %q, want prefix + %q", got, want)
	}
	if _, err := Parse(want); err != nil {
		t.Fatalf("Build output does not parse: %v", err)
	}
	// A large body needs more BodyLength digits than a small one.
	big := NewBuilder("FIX.4.4", "B").SetString(58, strings.Repeat("x", 123456))
	if _, err := Parse(big.Build()); err != nil {
		t.Fatalf("large message does not parse: %v", err)
	}
}

func TestBuilder_AppendBody(t *testing.T) {
	b := NewBuilder("FIX.4.4", "D").SetString(11, "ORD").SetString(115, "OBO").SetString(89, "SIG")
	b.SetSections([]int{115}, []int{89}, nil)
	body, err := b.AppendBody(nil, nil, func(tag int) bool { return tag == 34 || tag == 52 })
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "115=OBO\x0111=ORD\x0189=SIG\x01"; got != want {
		t.Errorf("AppendBody = %q, want %q", got, want)
	}
	b.SetInt(34, 9)
	_, err = b.AppendBody(nil, nil, func(tag int) bool { return tag == 34 })
	var rerr *ReservedTagError
	if !errors.As(err, &rerr) || rerr.Tag != 34 {
		t.Fatalf("AppendBody with a reserved tag set: err = %v, want a ReservedTagError for 34", err)
	}
}

func TestBuilder_ResetReuses(t *testing.T) {
	b := NewBuilder("FIX.4.4", "D")
	b.SetSections([]int{49}, nil, nil)
	resets := 0
	b.OnReset(func() { resets++ })
	build := func() []byte {
		return b.SetString(49, "S").SetString(11, "ORD").SetInt(38, 100).SetFloat(44, 10.25).AppendTo(nil)
	}
	first := build()
	b.Reset()
	if resets != 1 {
		t.Errorf("OnReset function called %d times, want 1", resets)
	}
	if got := build(); !bytes.Equal(got, first) {
		t.Errorf("after Reset: %q, want %q", got, first)
	}
	if b.MsgType() != "D" || b.BeginString() != "FIX.4.4" {
		t.Errorf("Reset lost MsgType %q or BeginString %q", b.MsgType(), b.BeginString())
	}
	buf := make([]byte, 0, 512)
	allocs := testing.AllocsPerRun(100, func() {
		b.Reset()
		b.SetString(49, "S").SetString(11, "ORD").SetInt(38, 100).SetFloat(44, 10.25)
		buf = b.AppendTo(buf[:0])
	})
	if allocs != 0 {
		t.Errorf("Reset, set and AppendTo: %v allocs, want 0", allocs)
	}
}

// Values stay correct when the arena grows and when a field is replaced.
func TestBuilder_ArenaGrowth(t *testing.T) {
	b := NewBuilder("FIX.4.4", "D")
	for i := range 200 {
		b.SetString(5000+i, strings.Repeat("v", i))
	}
	b.SetString(5000, "replaced")
	m, err := Parse(b.Build())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(m.Get(5000)); got != "replaced" {
		t.Errorf("5000 = %q", got)
	}
	for i := 1; i < 200; i++ {
		if got := string(m.Get(5000 + i)); got != strings.Repeat("v", i) {
			t.Fatalf("%d = %q", 5000+i, got)
		}
	}
}

func TestAppendHelpers(t *testing.T) {
	if got := string(AppendTagValue([]byte("x"), 58, "hi")); got != "x58=hi\x01" {
		t.Errorf("AppendTagValue(string) = %q", got)
	}
	if got := string(AppendTagValue(nil, 58, []byte("hi"))); got != "58=hi\x01" {
		t.Errorf("AppendTagValue([]byte) = %q", got)
	}
	if got := string(AppendTagInt(nil, 34, -12)); got != "34=-12\x01" {
		t.Errorf("AppendTagInt = %q", got)
	}
	msg := []byte("8=FIX.4.4\x019=5\x0135=0\x01")
	if got, want := CheckSum(msg), byte(checksumBlockGeneric(msg)%256); got != want {
		t.Errorf("CheckSum = %d, want %d", got, want)
	}
	full := AppendTrailer(append([]byte("junk"), msg...), 4)
	if _, err := Parse(full[4:]); err != nil {
		t.Errorf("AppendTrailer: %q does not parse: %v", full[4:], err)
	}
}

func BenchmarkBuilderAppendTo(b *testing.B) {
	bl := NewBuilder("FIX.4.4", "D")
	bl.SetSections([]int{49, 56, 34, 52}, nil, nil)
	buf := make([]byte, 0, 512)
	b.ReportAllocs()
	for b.Loop() {
		bl.Reset()
		bl.SetString(49, "SENDER").SetString(56, "TARGET").SetInt(34, 12345).
			SetString(52, "20231015-10:00:00.000").SetString(11, "order-12345").
			SetString(55, "AAPL").SetString(54, "1").SetInt(38, 100).SetString(40, "2").SetFloat(44, 150.5)
		buf = bl.AppendTo(buf[:0])
	}
}

// Fields the groups write are checked against the reserved tags too.
func TestBuilder_AppendBodyReservedInGroups(t *testing.T) {
	b := NewBuilder("FIX.4.4", "D")
	b.SetSections(nil, nil, func(dst []Field) []Field { return append(dst, Field{Tag: 34, Value: []byte("9")}) })
	got, err := b.AppendBody([]byte("x"), nil, func(tag int) bool { return tag == 34 })
	var rerr *ReservedTagError
	if !errors.As(err, &rerr) || rerr.Tag != 34 || string(got) != "x" {
		t.Fatalf("AppendBody = %q, %v; want nothing appended and a ReservedTagError for 34", got, err)
	}
}

// SOH ends a field: AppendBody refuses a value holding one unless it is a
// DATA field of the receiver's profile right after the LENGTH field
// announcing its length, and appends nothing.
func TestBuilder_AppendBodySOH(t *testing.T) {
	custom, err := NewProfile([]DataPair{{Length: 50001, Data: 50002}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		b    *Builder
		p    *Profile // nil for the default one
		tag  int      // refused, 0 for accepted
	}{
		{"string", NewBuilder("FIX.4.4", "D").SetString(11, "O1\x0158=forged"), nil, 11},
		{"bytes", NewBuilder("FIX.4.4", "D").Set(58, []byte("a\x01")), nil, 58},
		{"appended", NewBuilder("FIX.4.4", "D").AppendString(58, "\x01"), nil, 58},
		{"header", func() *Builder {
			b := NewBuilder("FIX.4.4", "D").SetString(115, "a\x01b")
			b.SetSections([]int{115}, nil, nil)
			return b
		}(), nil, 115},
		{"group", func() *Builder {
			b := NewBuilder("FIX.4.4", "D")
			b.SetGroups(func(dst []Field) []Field {
				return append(AppendCount(dst, 78, 1), Field{Tag: 79, Value: []byte("A\x01")})
			}, 78)
			return b
		}(), nil, 79},
		{"DATA", NewBuilder("FIX.4.4", "A").SetData(95, 96, []byte("a\x01b")), nil, 0},
		{"DATA set by hand", NewBuilder("FIX.4.4", "A").SetInt(95, 3).Set(96, []byte("a\x01b")), nil, 0},
		{"DATA with a wrong length", NewBuilder("FIX.4.4", "A").SetData(95, 96, []byte("a\x01b")).SetInt(95, 2), nil, 96},
		{"DATA after another field", NewBuilder("FIX.4.4", "A").SetInt(95, 3).SetString(58, "t").Set(96, []byte("a\x01b")), nil, 96},
		{"DATA tag not announced", NewBuilder("FIX.4.4", "A").Set(96, []byte("\x01")), nil, 96},
		// The receiver's profile decides, SetData whatever pair it is given.
		{"pair the profile has", NewBuilder("FIX.4.4", "A").SetData(50001, 50002, []byte("\x01")), custom, 0},
		{"pair the profile lacks", NewBuilder("FIX.4.4", "A").SetData(50001, 50002, []byte("\x01")), nil, 50002},
		{"default pair the profile lacks", NewBuilder("FIX.4.4", "D").SetInt(1401, 12).SetString(1402, "O1\x0158=forged"), custom, 1402},
		{"group pair the profile has", func() *Builder {
			b := NewBuilder("FIX.4.4", "D")
			b.SetGroups(func(dst []Field) []Field {
				return append(AppendCount(dst, 78, 1), Field{Tag: 50001, Value: []byte("3")}, Field{Tag: 50002, Value: []byte("a\x01b")})
			}, 78)
			return b
		}(), custom, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.b.AppendBody([]byte("x"), tc.p, nil)
			if tc.tag == 0 {
				if err != nil {
					t.Fatalf("AppendBody: %v", err)
				}
				return
			}
			var serr *SOHValueError
			if !errors.As(err, &serr) || serr.Tag != tc.tag || string(got) != "x" {
				t.Fatalf("AppendBody = %q, %v; want nothing appended and a SOHValueError for %d", got, err, tc.tag)
			}
		})
	}
}

// Build writes a value holding SOH as it is, as a test crafting a
// malformed message needs.
func TestBuilder_BuildWritesSOH(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").SetString(11, "O1\x0158=forged").Build()
	if !bytes.Contains(raw, []byte("\x0111=O1\x0158=forged\x01")) {
		t.Errorf("Build = %q", raw)
	}
	// Nor does it look for one: no error is made, to be dropped.
	b := NewBuilder("FIX.4.4", "D").SetString(11, "O1\x0158=forged")
	buf := make([]byte, 0, 256)
	if allocs := testing.AllocsPerRun(100, func() { buf = b.AppendTo(buf[:0]) }); allocs != 0 {
		t.Errorf("AppendTo of a value holding SOH: %v allocations, want 0", allocs)
	}
}
