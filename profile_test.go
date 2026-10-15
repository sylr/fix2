package fix2

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNewProfile_Errors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pairs []DataPair
		want  string
	}{
		{"zero tag", []DataPair{{0, 5}}, "tag 0 of pair 0/5 out of range"},
		{"negative tag", []DataPair{{5, -1}}, "tag -1 of pair 5/-1 out of range"},
		{"tag too large", []DataPair{{5, 1 << 20}}, "out of range 1-1048575"},
		{"same tag twice in a pair", []DataPair{{5, 5}}, "tag 5 is both the LENGTH and the DATA field"},
		{"tag in two pairs", []DataPair{{5, 6}, {6, 7}}, "tag 6 appears in more than one pair"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProfile(tc.pairs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewProfile(%v) error = %v, want it to contain %q", tc.pairs, err, tc.want)
			}
		})
	}
}

// InRange agrees with NewProfile on the tags it accepts.
func TestDataPair_InRange(t *testing.T) {
	for _, tc := range []struct {
		pair DataPair
		want bool
	}{
		{DataPair{1, 2}, true},
		{DataPair{1<<20 - 2, 1<<20 - 1}, true},
		{DataPair{0, 2}, false},
		{DataPair{1, -1}, false},
		{DataPair{1 << 20, 2}, false},
		{DataPair{1, 1 << 20}, false},
	} {
		if got := tc.pair.InRange(); got != tc.want {
			t.Errorf("%v.InRange() = %v, want %v", tc.pair, got, tc.want)
		}
		if _, err := NewProfile([]DataPair{tc.pair}); (err == nil) != tc.want {
			t.Errorf("NewProfile(%v) error = %v, InRange says %v", tc.pair, err, tc.want)
		}
	}
}

func TestProfile_Lookups(t *testing.T) {
	p, err := NewProfile([]DataPair{{1525, 1527}, {95, 96}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tag, dataTag, lengthTag int
	}{
		{1525, 1527, 0},
		{1527, 0, 1525},
		{95, 96, 0},
		{96, 0, 95},
		{354, 0, 0}, // in the default profile, not in p
		{-1, 0, 0},
		{1 << 30, 0, 0},
	} {
		if got := p.DataTag(tc.tag); got != tc.dataTag {
			t.Errorf("DataTag(%d) = %d, want %d", tc.tag, got, tc.dataTag)
		}
		if got := p.DataLengthTag(tc.tag); got != tc.lengthTag {
			t.Errorf("DataLengthTag(%d) = %d, want %d", tc.tag, got, tc.lengthTag)
		}
	}
	pairs := p.DataPairs()
	pairs[0].Data = 1 // the copy is the caller's
	if p.DataTag(1525) != 1527 {
		t.Error("DataPairs returned the profile's own slice")
	}
	if DefaultProfile().DataTag(354) != 355 || DefaultProfile().DataLengthTag(89) != 93 {
		t.Error("DefaultProfile does not hold the standard DATA fields")
	}
}

// A profile reads as DATA the fields its dictionary defines as DATA, which the
// default profile leaves out because dictionaries disagree on those tags.
func TestParseInto_Profile(t *testing.T) {
	raw := buildMsg("FIX.4.4", "35=B\x011525=5\x011527=a\x01b=c\x01")
	p, err := NewProfile([]DataPair{{1525, 1527}})
	if err != nil {
		t.Fatal(err)
	}
	var m Message
	if err := ParseInto(&m, raw, p); err != nil {
		t.Fatalf("ParseInto with the profile: %v", err)
	}
	if got := string(m.Get(1527)); got != "a\x01b=c" {
		t.Errorf("1527 = %q, want %q", got, "a\x01b=c")
	}
	if err := ParseInto(&m, raw, nil); err == nil {
		t.Error("ParseInto with the default profile read 1527 as DATA")
	}
}

// Parsing into the same Message reuses its storage, also once it has grown
// beyond the inline buffer.
func TestParseInto_DoesNotAllocate(t *testing.T) {
	var b strings.Builder
	b.WriteString("35=D\x01")
	for i := range 60 {
		fmt.Fprintf(&b, "%d=v\x01", 5000+i)
	}
	for _, raw := range [][]byte{benchMsg(0), buildMsg("FIX.4.4", b.String())} {
		var m Message
		if err := ParseInto(&m, raw, nil); err != nil {
			t.Fatal(err)
		}
		allocs := testing.AllocsPerRun(100, func() {
			if err := ParseInto(&m, raw, nil); err != nil {
				t.Fatal(err)
			}
		})
		if allocs != 0 {
			t.Errorf("ParseInto of %d fields: %v allocs, want 0", m.Count(), allocs)
		}
	}
}

// A copy of a Message points at the original's inline buffer: parsing into
// the copy must not overwrite the original's fields.
func TestParseInto_CopiedMessage(t *testing.T) {
	var orig Message
	if err := ParseInto(&orig, buildMsg("FIX.4.4", "35=0\x0149=ORIG\x01"), nil); err != nil {
		t.Fatal(err)
	}
	cp := orig
	if err := ParseInto(&cp, buildMsg("FIX.4.4", "35=0\x0149=COPY\x01"), nil); err != nil {
		t.Fatal(err)
	}
	if got := string(orig.Get(49)); got != "ORIG" {
		t.Errorf("parsing into a copy changed the original: 49 = %q", got)
	}
	if got := string(cp.Get(49)); got != "COPY" {
		t.Errorf("copy: 49 = %q, want COPY", got)
	}
}

// A plain copy shares the original's storage, which ParseInto reuses; Clone
// does not.
func TestParseInto_CopyAliasesCloneDoesNot(t *testing.T) {
	var b strings.Builder
	b.WriteString("35=D\x01")
	for i := range 40 {
		fmt.Fprintf(&b, "%d=v\x01", 5000+i)
	}
	for _, body := range []string{"35=0\x0149=ORIG\x01", b.String() + "49=ORIG\x01"} {
		var m Message
		raw := buildMsg("FIX.4.4", body)
		if err := ParseInto(&m, raw, nil); err != nil {
			t.Fatal(err)
		}
		cp, cl := m, m.Clone()
		if err := ParseInto(&m, buildMsg("FIX.4.4", "35=0\x0149=NEXT\x01"), nil); err != nil {
			t.Fatal(err)
		}
		if got := string(cp.Get(49)); got != "NEXT" {
			t.Errorf("%d fields: copy reads 49 = %q after reuse, want the aliasing documented (NEXT)", cl.Count(), got)
		}
		for i := range raw {
			raw[i] = 'z'
		}
		if got := string(cl.Get(49)); got != "ORIG" {
			t.Errorf("%d fields: clone reads 49 = %q, want ORIG", cl.Count(), got)
		}
		if cl.Count() != len(cl.Fields()) || string(cl.MsgType()) != string(body[3:4]) {
			t.Errorf("clone = %v", cl.Fields())
		}
	}
}

func TestParseInto_Error(t *testing.T) {
	var m Message
	err := ParseInto(&m, buildMsg("FIX.4.4", "35=0\x0149=A\x01x=1\x01"), nil)
	if !errors.Is(err, ErrInvalidTag) {
		t.Fatalf("ParseInto error = %v, want ErrInvalidTag", err)
	}
	if got := string(m.Get(49)); got != "A" {
		t.Errorf("fields parsed before the error: 49 = %q, want %q", got, "A")
	}
}

func BenchmarkParseInto(b *testing.B) {
	raw := benchMsg(10)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	var m Message
	for b.Loop() {
		if err := ParseInto(&m, raw, nil); err != nil {
			b.Fatal(err)
		}
	}
}
