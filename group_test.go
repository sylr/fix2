package fix2

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestGroupsBasic(t *testing.T) {
	// Build a message with two NoAllocs group entries:
	// 78=2|79=ACC1|80=500|79=ACC2|80=300|
	raw := NewBuilder("FIX.4.4", "D").
		SetString(11, "ORD-001").
		SetInt(78, 2).
		AppendString(79, "ACC1").
		AppendFloat(80, 500).
		AppendString(79, "ACC2").
		AppendFloat(80, 300).
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, []int{79, 80})
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// First entry
	if got := string(groups[0].Get(79)); got != "ACC1" {
		t.Errorf("group[0] tag 79 = %q, want %q", got, "ACC1")
	}
	if got := string(groups[0].Get(80)); got != "500" {
		t.Errorf("group[0] tag 80 = %q, want %q", got, "500")
	}

	// Second entry
	if got := string(groups[1].Get(79)); got != "ACC2" {
		t.Errorf("group[1] tag 79 = %q, want %q", got, "ACC2")
	}
	if got := string(groups[1].Get(80)); got != "300" {
		t.Errorf("group[1] tag 80 = %q, want %q", got, "300")
	}
}

func TestGroupsZeroCount(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").
		SetString(11, "ORD-001").
		SetInt(78, 0).
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, []int{79, 80})
	if len(groups) != 0 {
		t.Fatalf("expected 0 groups, got %d", len(groups))
	}
}

func TestGroupsMissingCounter(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").
		SetString(11, "ORD-001").
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, []int{79, 80})
	if groups != nil {
		t.Fatalf("expected nil groups, got %v", groups)
	}
}

func TestGroupFields(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").
		SetInt(78, 1).
		AppendString(79, "ACC1").
		AppendFloat(80, 100).
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, []int{79, 80})
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}

	fields := groups[0].Fields()
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}
}

func TestGroupGetMissing(t *testing.T) {
	g := Group{fields: []Field{{Tag: 79, Value: []byte("ACC1")}}}
	if got := g.Get(999); got != nil {
		t.Errorf("expected nil for missing tag, got %q", got)
	}
}

func TestGroupNestedGroups(t *testing.T) {
	// Build a message with 1 parent group entry containing 2 nested group entries:
	// 78=1|79=ACC1|80=500|5000=2|5001=INST1|5002=CODE1|5001=INST2|5002=CODE2|
	raw := NewBuilder("FIX.4.4", "D").
		SetString(11, "ORD-001").
		SetInt(78, 1).
		AppendString(79, "ACC1").
		AppendFloat(80, 500).
		AppendInt(5000, 2).
		AppendString(5001, "INST1").
		AppendString(5002, "CODE1").
		AppendString(5001, "INST2").
		AppendString(5002, "CODE2").
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	parentChildTags := []int{79, 80, 5000, 5001, 5002}
	groups := msg.Groups(78, parentChildTags)
	if len(groups) != 1 {
		t.Fatalf("expected 1 parent group, got %d", len(groups))
	}

	// The parent group should contain all fields including nested.
	if got := string(groups[0].Get(79)); got != "ACC1" {
		t.Errorf("parent tag 79 = %q, want %q", got, "ACC1")
	}

	// Extract nested groups from the parent group entry.
	nestedChildTags := []int{5001, 5002}
	nested := groups[0].Groups(5000, nestedChildTags)
	if len(nested) != 2 {
		t.Fatalf("expected 2 nested groups, got %d", len(nested))
	}

	if got := string(nested[0].Get(5001)); got != "INST1" {
		t.Errorf("nested[0] tag 5001 = %q, want %q", got, "INST1")
	}
	if got := string(nested[0].Get(5002)); got != "CODE1" {
		t.Errorf("nested[0] tag 5002 = %q, want %q", got, "CODE1")
	}
	if got := string(nested[1].Get(5001)); got != "INST2" {
		t.Errorf("nested[1] tag 5001 = %q, want %q", got, "INST2")
	}
	if got := string(nested[1].Get(5002)); got != "CODE2" {
		t.Errorf("nested[1] tag 5002 = %q, want %q", got, "CODE2")
	}
}

func TestGroupsNilChildTags(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").
		SetInt(78, 1).
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, nil)
	if groups != nil {
		t.Fatalf("expected nil groups, got %v", groups)
	}
}

// fieldsMsg builds a Message from "tag=value" pairs without parsing.
func fieldsMsg(t *testing.T, pairs ...string) *Message {
	t.Helper()
	var m Message
	m.init()
	for _, p := range pairs {
		tag, val, ok := strings.Cut(p, "=")
		if !ok {
			t.Fatalf("bad pair %q", p)
		}
		n, err := strconv.Atoi(tag)
		if err != nil {
			t.Fatal(err)
		}
		m.append(Field{Tag: n, Value: []byte(val)})
	}
	return &m
}

// entryTags returns the tags of each entry.
func entryTags(groups []Group) [][]int {
	var out [][]int
	for _, g := range groups {
		var tags []int
		for _, f := range g.Fields() {
			tags = append(tags, f.Tag)
		}
		out = append(out, tags)
	}
	return out
}

func TestGroupsOfNestedSharesParentDelimiter(t *testing.T) {
	// The nested group uses the parent's delimiter (100): a flat tag list
	// cannot tell the levels apart, but counting the nested entries can.
	nested := &GroupSpec{Counter: 200, Delim: 100, Tags: []int{100, 101}}
	parent := &GroupSpec{Counter: 900, Delim: 100, Tags: []int{100}, Nested: []*GroupSpec{nested}}
	m := fieldsMsg(t, "900=2", "100=P1", "200=1", "100=C1", "101=x", "100=P2", "200=0", "55=after")

	entries := m.GroupsOf(parent)
	if got, want := entryTags(entries), [][]int{{100, 200, 100, 101}, {100, 200}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parent entries = %v, want %v", got, want)
	}
	if got, want := entryTags(entries[0].GroupsOf(nested)), [][]int{{100, 101}}; !reflect.DeepEqual(got, want) {
		t.Errorf("nested entries = %v, want %v", got, want)
	}
	if got := entries[1].GroupsOf(nested); len(got) != 0 {
		t.Errorf("empty nested group = %v", entryTags(got))
	}
}

func TestGroupsOfDelimiterIsNestedCounter(t *testing.T) {
	// Like FIX 5.0 SP2 NoAsgnReqs, whose entries start with the Parties
	// component: the delimiter is the nested NoPartyIDs counter.
	parties := &GroupSpec{Counter: 453, Delim: 448, Tags: []int{448, 447, 452}}
	reqs := &GroupSpec{Counter: 1499, Delim: 453, Tags: []int{453, 1500}, Nested: []*GroupSpec{parties}}
	m := fieldsMsg(t, "1499=2",
		"453=1", "448=A", "447=D", "452=1", "1500=x",
		"453=2", "448=B", "448=C", "1500=y")

	entries := m.GroupsOf(reqs)
	if len(entries) != 2 {
		t.Fatalf("got %d entries %v, want 2", len(entries), entryTags(entries))
	}
	for i, want := range []int{1, 2} {
		if got := len(entries[i].GroupsOf(parties)); got != want {
			t.Errorf("entry %d: %d parties, want %d", i, got, want)
		}
	}
	if got := string(entries[1].Get(1500)); got != "y" {
		t.Errorf("entry 1 tag 1500 = %q", got)
	}
}

func TestGroupsOfBoundaries(t *testing.T) {
	spec := &GroupSpec{Counter: 78, Delim: 79, Tags: []int{79, 80}}
	for _, tc := range []struct {
		name  string
		pairs []string
		want  [][]int
	}{
		{"stops at a foreign tag", []string{"78=2", "79=a", "80=1", "58=t", "79=b"}, [][]int{{79, 80}}},
		{"stops at the count", []string{"78=1", "79=a", "79=b"}, [][]int{{79}}},
		{"entry must start with the delimiter", []string{"78=1", "80=1", "79=a"}, nil},
		{"no counter", []string{"79=a"}, nil},
		{"invalid count", []string{"78=x", "79=a"}, nil},
		{"negative count", []string{"78=-1", "79=a"}, nil},
	} {
		if got := entryTags(fieldsMsg(t, tc.pairs...).GroupsOf(spec)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGroupsOfZeroCopy(t *testing.T) {
	spec := &GroupSpec{Counter: 78, Delim: 79, Tags: []int{79, 80}}
	m := fieldsMsg(t, "78=1", "79=a", "80=1")
	entries := m.GroupsOf(spec)
	if len(entries) != 1 || &entries[0].Fields()[0] != &m.Fields()[1] {
		t.Error("entry fields should be a sub-slice of the message fields")
	}
	// Appending to an entry must not overwrite the message's next field.
	_ = append(entries[0].Fields(), Field{Tag: 1})
	if m.Fields()[2].Tag != 80 {
		t.Error("appending to an entry overwrote message fields")
	}
}

func TestGroupsOfNestedDoesNotAllocate(t *testing.T) {
	// One parent entry holding 32 one-entry nested groups: skipping nested
	// blocks must not build their entries.
	parent := &GroupSpec{Counter: 900, Delim: 100, Tags: []int{100}}
	pairs := []string{"900=1", "100=p"}
	for i := range 32 {
		counter, delim := 1000+2*i, 1001+2*i
		parent.Nested = append(parent.Nested, &GroupSpec{Counter: counter, Delim: delim, Tags: []int{delim}})
		pairs = append(pairs, fmt.Sprintf("%d=1", counter), fmt.Sprintf("%d=x", delim))
	}
	m := fieldsMsg(t, pairs...)
	if got := len(m.GroupsOf(parent)); got != 1 {
		t.Fatalf("got %d entries, want 1", got)
	}
	if allocs := testing.AllocsPerRun(100, func() { m.GroupsOf(parent) }); allocs > 1 {
		t.Errorf("GroupsOf allocated %v times, want at most 1 (the result)", allocs)
	}
}

func TestGroupSpecChild(t *testing.T) {
	child := &GroupSpec{Counter: 2}
	s := &GroupSpec{Counter: 1, Nested: []*GroupSpec{child}}
	if s.Child(2) != child || s.Child(3) != nil {
		t.Error("Child lookup")
	}
	var none *GroupSpec
	if none.Child(2) != nil {
		t.Error("Child on a nil spec must return nil")
	}
}

func TestGetTopLevel(t *testing.T) {
	hops := &GroupSpec{Counter: 627, Delim: 628, Tags: []int{628}}
	// HopCompID (628) occurs in the NoHops group first, then directly.
	m := fieldsMsg(t, "627=2", "628=hop1", "628=hop2", "112=t", "628=direct")
	if got := string(m.GetTopLevel(628, hops)); got != "direct" {
		t.Errorf("GetTopLevel = %q, want direct", got)
	}
	if got := string(m.Get(628)); got != "hop1" {
		t.Errorf("Get = %q, want hop1 (first occurrence)", got)
	}
	if got := m.GetTopLevel(999, hops); got != nil {
		t.Errorf("absent tag = %q", got)
	}
}

func TestGroupsOfRepeatedTagEndsEntry(t *testing.T) {
	// 629 belongs to NoHops entries, but may appear only once per entry: the
	// second 629 is the message's own field after the group.
	hops := &GroupSpec{Counter: 627, Delim: 628, Tags: []int{628, 629}}
	m := fieldsMsg(t, "627=1", "628=hop", "629=group", "629=direct", "112=t")
	if got := entryTags(m.GroupsOf(hops)); !reflect.DeepEqual(got, [][]int{{628, 629}}) {
		t.Errorf("entries = %v", got)
	}
	if got := string(m.GetTopLevel(629, hops)); got != "direct" {
		t.Errorf("GetTopLevel(629) = %q, want direct", got)
	}
}

func TestGroupsOfRepeatedNestedCounterEndsEntry(t *testing.T) {
	child := &GroupSpec{Counter: 200, Delim: 201, Tags: []int{201}}
	parent := &GroupSpec{Counter: 900, Delim: 100, Tags: []int{100}, Nested: []*GroupSpec{child}}
	// The second 200 is the message's own field after the parent group.
	m := fieldsMsg(t, "900=1", "100=p", "200=1", "201=c", "200=0")
	if got := entryTags(m.GroupsOf(parent)); !reflect.DeepEqual(got, [][]int{{100, 200, 201}}) {
		t.Errorf("entries = %v", got)
	}
	if got := string(m.GetTopLevel(200, parent)); got != "0" {
		t.Errorf("GetTopLevel(200) = %q, want 0", got)
	}
}

func TestGroupsOfRepeatedNestedCounterManyChildren(t *testing.T) {
	// The repeated child sits at nested index n-1, past the first 64 and,
	// with 200 tags, past the 256 bits kept on the stack.
	for _, c := range []struct{ tags, nested int }{{1, 64}, {1, 65}, {1, 100}, {200, 55}, {200, 56}, {200, 57}, {200, 70}} {
		tags := make([]int, c.tags)
		for i := range tags {
			tags[i] = 100 + i
		}
		parent := &GroupSpec{Counter: 900, Delim: 100, Tags: tags}
		for i := range c.nested {
			parent.Nested = append(parent.Nested, &GroupSpec{Counter: 2000 + 2*i, Delim: 2001 + 2*i, Tags: []int{2001 + 2*i}})
		}
		counter := 2000 + 2*(c.nested-1)
		m := fieldsMsg(t, "900=1", "100=p",
			fmt.Sprintf("%d=1", counter), fmt.Sprintf("%d=c", counter+1), fmt.Sprintf("%d=0", counter))
		if got := entryTags(m.GroupsOf(parent)); !reflect.DeepEqual(got, [][]int{{100, counter, counter + 1}}) {
			t.Errorf("%d tags, %d nested: entries = %v", c.tags, c.nested, got)
		}
		if got := string(m.GetTopLevel(counter, parent)); got != "0" {
			t.Errorf("%d tags, %d nested: GetTopLevel(%d) = %q, want 0", c.tags, c.nested, counter, got)
		}
		if allocs := testing.AllocsPerRun(100, func() { m.GetTopLevel(counter, parent) }); c.tags+c.nested <= 256 && allocs != 0 {
			t.Errorf("%d tags, %d nested: GetTopLevel allocated %v times", c.tags, c.nested, allocs)
		}
	}
}

func TestGroupsOfRepeatedZeroCountCounter(t *testing.T) {
	// A first occurrence with count 0 still counts: the second ends the entry.
	child := &GroupSpec{Counter: 200, Delim: 201, Tags: []int{201}}
	parent := &GroupSpec{Counter: 900, Delim: 100, Tags: []int{100}, Nested: []*GroupSpec{child}}
	m := fieldsMsg(t, "900=1", "100=p", "200=0", "200=0")
	if got := entryTags(m.GroupsOf(parent)); !reflect.DeepEqual(got, [][]int{{100, 200}}) {
		t.Errorf("entries = %v", got)
	}
	if got := string(m.GetTopLevel(200, parent)); got != "0" {
		t.Errorf("GetTopLevel(200) = %q, want 0", got)
	}
}

func TestGroupsOfRepeatedGrandchildCounter(t *testing.T) {
	// The repeated 300 ends the child entry, then the parent entry (300 is
	// not a parent tag), leaving it to the message.
	grandchild := &GroupSpec{Counter: 300, Delim: 301, Tags: []int{301}}
	child := &GroupSpec{Counter: 200, Delim: 201, Tags: []int{201}, Nested: []*GroupSpec{grandchild}}
	parent := &GroupSpec{Counter: 900, Delim: 100, Tags: []int{100}, Nested: []*GroupSpec{child}}
	m := fieldsMsg(t, "900=1", "100=p", "200=1", "201=c", "300=1", "301=g", "300=0")
	if got := entryTags(m.GroupsOf(parent)); !reflect.DeepEqual(got, [][]int{{100, 200, 201, 300, 301}}) {
		t.Errorf("entries = %v", got)
	}
	children := m.GroupsOf(parent)[0].GroupsOf(child)
	if got := entryTags(children); !reflect.DeepEqual(got, [][]int{{201, 300, 301}}) {
		t.Errorf("child entries = %v", got)
	}
	if got := string(m.GetTopLevel(300, parent)); got != "0" {
		t.Errorf("GetTopLevel(300) = %q, want 0", got)
	}
}

func TestGroupsOfLargeSpec(t *testing.T) {
	// Specs above 256 tags use a heap bitset; repetition is still detected
	// for tags beyond index 255.
	for _, n := range []int{256, 257, 300} {
		tags := make([]int, n)
		for i := range tags {
			tags[i] = 1000 + i
		}
		spec := &GroupSpec{Counter: 1, Delim: 1000, Tags: tags}
		last := fmt.Sprintf("%d=x", 1000+n-1)
		m := fieldsMsg(t, "1=1", "1000=d", last, last)
		got := m.GroupsOf(spec)
		if len(got) != 1 || len(got[0].Fields()) != 2 {
			t.Errorf("%d tags: entries = %v, want one entry ending before the repeated tag", n, entryTags(got))
		}
	}
}

func TestTopLevelFields(t *testing.T) {
	child := &GroupSpec{Counter: 200, Delim: 201, Tags: []int{201, 202}}
	parent := &GroupSpec{Counter: 900, Delim: 100, Tags: []int{100, 101}, Nested: []*GroupSpec{child}}
	m := fieldsMsg(t, "35=M", "900=2", "100=a", "200=1", "201=c", "202=c", "100=b", "101=b", "101=direct", "200=0")
	var tags []int
	for f := range m.TopLevelFields(parent) {
		tags = append(tags, f.Tag)
	}
	if want := []int{35, 900, 101, 200}; !reflect.DeepEqual(tags, want) {
		t.Errorf("TopLevelFields = %v, want %v", tags, want)
	}
	// The counter itself is top level.
	if got := string(m.GetTopLevel(900, parent)); got != "2" {
		t.Errorf("GetTopLevel(900) = %q, want 2", got)
	}
	// Stopping early.
	for f := range m.TopLevelFields(parent) {
		if f.Tag != 35 {
			t.Errorf("first field = %d", f.Tag)
		}
		break
	}
}
