package fix2

import (
	"iter"
	"slices"
	"sync"
	"sync/atomic"
)

// Group represents a single instance within a FIX repeating group.
type Group struct {
	fields []Field
}

// Get returns the value of the first field with the given tag, or nil if not found.
func (g *Group) Get(tag int) []byte {
	for i := range g.fields {
		if g.fields[i].Tag == tag {
			return g.fields[i].Value
		}
	}
	return nil
}

// Fields returns the fields in this group instance.
func (g *Group) Fields() []Field {
	return g.fields
}

// Groups extracts nested repeating group instances from this group entry.
// counterTag is the NUMINGROUP tag and childTags lists all tags belonging to
// the nested group. childTags[0] is the delimiter tag that starts each instance.
func (g *Group) Groups(counterTag int, childTags []int) []Group {
	if len(childTags) == 0 {
		return nil
	}
	delimTag := childTags[0]

	counterIdx := -1
	count := 0
	for i := range g.fields {
		if g.fields[i].Tag == counterTag {
			counterIdx = i
			count, _ = ParseInt(g.fields[i].Value)
			break
		}
	}
	if counterIdx < 0 || count == 0 {
		return nil
	}

	var groups []Group
	var cur []Field
	for i := counterIdx + 1; i < len(g.fields) && len(groups) < count; i++ {
		f := g.fields[i]
		if !slices.Contains(childTags, f.Tag) {
			break
		}
		if f.Tag == delimTag {
			if cur != nil {
				groups = append(groups, Group{fields: cur})
			}
			cur = []Field{f}
		} else if cur != nil {
			cur = append(cur, f)
		}
	}
	if cur != nil && len(groups) < count {
		groups = append(groups, Group{fields: cur})
	}
	return groups
}

// GroupSpec describes a repeating group for GroupsOf: its counter, the
// delimiter every entry starts with, the tags an entry holds directly and the
// groups it can contain. A group's fields can differ between messages, so
// generated code declares a spec tree per message group, from that message's
// own definition. A spec must not be changed or copied once used: large
// ones build an index of their tags on first use, which go vet's copylocks
// check guards.
type GroupSpec struct {
	Counter int          // NUMINGROUP tag
	Delim   int          // first tag of every entry
	Tags    []int        // tags an entry holds directly, Delim included
	Nested  []*GroupSpec // groups an entry can contain

	// index maps a tag to its position in Tags, or to -1-i for the counter of
	// Nested[i], for specs of at least indexedSpec tags and groups.
	index atomic.Pointer[map[int]int]
}

// indexedSpec is the number of tags and nested groups from which a spec
// looks tags up in an index rather than scanning: below it, scanning is as
// fast, and a crafted dictionary cannot make a wide group cost quadratic
// time per entry above it.
const indexedSpec = 16

// find returns the position of tag in s.Tags, or -1, and the index in
// s.Nested of the group whose counter is tag, or -1. A nested counter takes
// precedence.
func (s *GroupSpec) find(tag int) (k, child int) {
	if len(s.Tags)+len(s.Nested) < indexedSpec {
		if child = s.childIndex(tag); child >= 0 {
			return -1, child
		}
		return slices.Index(s.Tags, tag), -1
	}
	idx := s.index.Load()
	if idx == nil {
		m := make(map[int]int, len(s.Tags)+len(s.Nested))
		for k := len(s.Tags) - 1; k >= 0; k-- { // the first position wins
			m[s.Tags[k]] = k
		}
		for c := len(s.Nested) - 1; c >= 0; c-- {
			m[s.Nested[c].Counter] = -1 - c
		}
		s.index.CompareAndSwap(nil, &m)
		idx = s.index.Load()
	}
	switch v, ok := (*idx)[tag]; {
	case !ok:
		return -1, -1
	case v < 0:
		return -1, -1 - v
	default:
		return v, -1
	}
}

// GroupsOf extracts the entries of the group described by spec. Unlike
// Groups, it knows the group hierarchy: a nested group is consumed as a block
// of exactly as many entries as its counter says, so tags it shares with the
// enclosing group cannot end an entry early. Entries are sub-slices of the
// message's fields.
//
// others are the message's other groups: their entries are skipped while
// looking for spec's counter, which one of them may also hold in a nested
// group of the same name.
func (m *Message) GroupsOf(spec *GroupSpec, others ...*GroupSpec) []Group {
	return groupsOf(m.fields, spec, others)
}

// GroupsOf extracts the entries of a group nested in this entry; see
// Message.GroupsOf. others are typically the entry spec's other Nested
// groups.
func (g *Group) GroupsOf(spec *GroupSpec, others ...*GroupSpec) []Group {
	return groupsOf(g.fields, spec, others)
}

func groupsOf(fields []Field, spec *GroupSpec, others []*GroupSpec) []Group {
	for i := 0; i < len(fields); {
		f := fields[i]
		if f.Tag == spec.Counter {
			count, err := ParseInt(f.Value)
			if err != nil || count <= 0 {
				return nil
			}
			groups, _ := spec.entries(fields, i+1, count)
			return groups
		}
		if o := groupSpecFor(others, f.Tag); o != nil {
			count, _ := ParseInt(f.Value)
			i = o.skip(fields, i+1, count)
			continue
		}
		i++
	}
	return nil
}

// entries reads up to count entries starting at fields[i] and returns them
// with the index just past the block.
func (s *GroupSpec) entries(fields []Field, i, count int) ([]Group, int) {
	var groups []Group
	wide := s.wideSeen()
	defer releaseSeen(wide)
	for len(groups) < count && i < len(fields) && fields[i].Tag == s.Delim {
		start := i
		i = s.entry(fields, i, wide)
		// Cap the capacity so appending to an entry cannot overwrite the
		// fields that follow it.
		groups = append(groups, Group{fields: fields[start:i:i]})
	}
	return groups, i
}

// entry consumes the entry whose delimiter is at fields[i] and returns the
// index just past it. FIX does not allow a tag twice in one entry, so a
// repeated tag ends it: the field belongs to the next entry or to the
// enclosing message.
//
// wide is the zeroed bitset of a spec of more than 256 tags and groups (see
// wideSeen), or nil; entry leaves it zeroed.
func (s *GroupSpec) entry(fields []Field, i int, wide []uint64) int {
	// Tags seen in this entry, as bits indexed by position in s.Tags, then
	// nested counters by position in s.Nested: on the stack for groups of up
	// to 256 of them. A wider spec's bitset is shared by its entries, and
	// only the words an entry sets are cleared, so that many small entries
	// do not cost the spec's width each.
	var small [4]uint64
	seen := small[:]
	var touchedBuf [16]int
	touched := touchedBuf[:0]
	if wide != nil {
		seen = wide
	}
	end := len(fields)
	for first := true; i < len(fields); first = false {
		tag := fields[i].Tag
		if tag == s.Delim && !first {
			end = i // the next entry starts
			break
		}
		k, c := s.find(tag)
		switch {
		case c >= 0:
			// A nested counter, possibly the delimiter itself: it cannot
			// repeat within the entry either, then skip its entries by count.
			k = len(s.Tags) + c
		case k < 0 && !first:
			end = i // not part of this group
		}
		if end != len(fields) {
			break
		}
		if k >= 0 {
			if seen[k/64]&(1<<(k%64)) != 0 {
				end = i // a tag cannot repeat within an entry
				break
			}
			if wide != nil && seen[k/64] == 0 {
				touched = append(touched, k/64)
			}
			seen[k/64] |= 1 << (k % 64)
		}
		if c >= 0 {
			count, _ := ParseInt(fields[i].Value)
			i = s.Nested[c].skip(fields, i+1, count)
			continue
		}
		i++
	}
	if i < end {
		end = i
	}
	for _, w := range touched {
		seen[w] = 0
	}
	return end
}

// skip returns the index just past up to count entries starting at
// fields[i], like entries without building them.
func (s *GroupSpec) skip(fields []Field, i, count int) int {
	wide := s.wideSeen()
	defer releaseSeen(wide)
	for n := 0; n < count && i < len(fields) && fields[i].Tag == s.Delim; n++ {
		i = s.entry(fields, i, wide)
	}
	return i
}

// seenPool holds the zeroed bitsets of wide specs between uses.
var seenPool sync.Pool

// wideSeen returns a zeroed bitset for the entries of a spec of more than 256
// tags and nested groups, or nil for a narrower one; releaseSeen returns it,
// zeroed, to seenPool.
func (s *GroupSpec) wideSeen() []uint64 {
	n := (len(s.Tags) + len(s.Nested) + 63) / 64
	if n <= 4 {
		return nil
	}
	if p, ok := seenPool.Get().(*[]uint64); ok && cap(*p) >= n {
		return (*p)[:n]
	}
	return make([]uint64, n)
}

func releaseSeen(seen []uint64) {
	if seen != nil {
		p := new([]uint64)
		*p = seen
		seenPool.Put(p)
	}
}

// childIndex returns the index in s.Nested of the group whose counter is tag,
// or -1.
func (s *GroupSpec) childIndex(tag int) int {
	for i, n := range s.Nested {
		if n.Counter == tag {
			return i
		}
	}
	return -1
}

// Child returns the spec of the nested group whose counter is tag, or nil if
// there is none (including when s is nil).
func (s *GroupSpec) Child(counter int) *GroupSpec {
	if s == nil {
		return nil
	}
	for _, n := range s.Nested {
		if n.Counter == counter {
			return n
		}
	}
	return nil
}

// GetTopLevel returns the value of the first field with the given tag that is
// not inside one of the groups described by specs, or nil. Use it for a tag
// that a message holds both directly and inside a repeating group: Get would
// return whichever occurrence comes first.
func (m *Message) GetTopLevel(tag int, specs ...*GroupSpec) []byte {
	return getTopLevel(m.fields, tag, specs)
}

// GetTopLevel returns the value of the first field with the given tag in this
// entry that is not inside one of the nested groups described by specs, or
// nil; see Message.GetTopLevel.
func (g *Group) GetTopLevel(tag int, specs ...*GroupSpec) []byte {
	return getTopLevel(g.fields, tag, specs)
}

func getTopLevel(fields []Field, tag int, specs []*GroupSpec) []byte {
	for f := range topLevelFields(fields, specs, nil) {
		if f.Tag == tag {
			return f.Value
		}
	}
	return nil
}

// TopLevelFields returns an iterator over the message's fields that are not
// inside one of the groups described by specs. Group counters are top level;
// the entries they announce are skipped as blocks, as GroupsOf reads them.
func (m *Message) TopLevelFields(specs ...*GroupSpec) iter.Seq[Field] {
	return topLevelFields(m.fields, specs, nil)
}

// TopLevelFieldsFunc is TopLevelFields with the groups given by lookup,
// which returns the spec of the group whose counter is tag, or nil: for many
// groups, a lookup in a table is faster than scanning a list of specs.
func (m *Message) TopLevelFieldsFunc(lookup func(tag int) *GroupSpec) iter.Seq[Field] {
	return topLevelFields(m.fields, nil, lookup)
}

// TopLevelFields returns an iterator over the entry's fields that are not
// inside one of the nested groups described by specs; see
// Message.TopLevelFields.
func (g *Group) TopLevelFields(specs ...*GroupSpec) iter.Seq[Field] {
	return topLevelFields(g.fields, specs, nil)
}

// topLevelFields iterates over the fields outside the groups found by lookup
// or, if it is nil, in specs.
func topLevelFields(fields []Field, specs []*GroupSpec, lookup func(int) *GroupSpec) iter.Seq[Field] {
	return func(yield func(Field) bool) {
		for i := 0; i < len(fields); {
			f := fields[i]
			if !yield(f) {
				return
			}
			var spec *GroupSpec
			if lookup != nil {
				spec = lookup(f.Tag)
			} else {
				spec = groupSpecFor(specs, f.Tag)
			}
			if spec != nil {
				count, _ := ParseInt(f.Value)
				i = spec.skip(fields, i+1, count)
				continue
			}
			i++
		}
	}
}

// groupSpecFor returns the spec whose counter is tag, or nil.
func groupSpecFor(specs []*GroupSpec, tag int) *GroupSpec {
	for _, s := range specs {
		if s.Counter == tag {
			return s
		}
	}
	return nil
}
