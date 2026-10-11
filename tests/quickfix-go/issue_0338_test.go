package quickfixgo

import (
	"bytes"
	"slices"
	"testing"

	fix2 "github.com/sylr/fix2"
)

// https://github.com/quickfixgo/quickfix/issues/338
//
// Messages are pooled, so keeping one takes a deep copy, but CopyInto shared
// the raw message with the original: the copy's groups read corrupted data
// once the original was reused. A clone must own everything it reads.
func TestIssue338_CloneIsDeep(t *testing.T) {
	data := raw("8=FIX.4.4|35=d|34=2|49=A|52=20240101-00:00:00.000|56=B|320=REQ|322=RSP|323=1|55=SYM|" +
		"453=2|448=P1|447=D|452=1|802=1|523=S1|803=2|448=P2|447=D|452=3|58=end")
	spec := &fix2.GroupSpec{Counter: 453, Delim: 448, Tags: []int{448, 447, 452},
		Nested: []*fix2.GroupSpec{{Counter: 802, Delim: 523, Tags: []int{523, 803}}}}
	var m fix2.Message
	if err := fix2.ParseInto(&m, data, nil); err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(m.Fields())
	for i := range want {
		want[i].Value = bytes.Clone(want[i].Value)
	}
	c := m.Clone()

	// The original's buffer is reused, and the original parsed into again.
	copy(data, bytes.Repeat([]byte("X"), len(data)))
	if err := fix2.ParseInto(&m, raw("8=FIX.4.4|35=0|34=3|49=A|52=20240101-00:00:01.000|56=B|112=TEST"), nil); err != nil {
		t.Fatal(err)
	}

	if !slices.EqualFunc(c.Fields(), want, func(a, b fix2.Field) bool { return a.Tag == b.Tag && bytes.Equal(a.Value, b.Value) }) {
		t.Fatalf("clone changed: got %v, want %v", c.Fields(), want)
	}
	parties := c.GroupsOf(spec)
	if len(parties) != 2 || string(parties[0].Get(448)) != "P1" || string(parties[1].Get(448)) != "P2" {
		t.Fatalf("clone's parties %v", parties)
	}
	if subs := parties[0].GroupsOf(spec.Nested[0]); len(subs) != 1 || string(subs[0].Get(523)) != "S1" {
		t.Fatalf("clone's party sub IDs %v", subs)
	}
	if got := string(c.Get(58)); got != "end" {
		t.Errorf("Text %q, want end", got)
	}
}
