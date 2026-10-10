package fix2

import (
	"bytes"
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
