package fix2

import (
	"iter"
	"slices"
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
// own definition.
type GroupSpec struct {
	Counter int          // NUMINGROUP tag
	Delim   int          // first tag of every entry
	Tags    []int        // tags an entry holds directly, Delim included
	Nested  []*GroupSpec // groups an entry can contain
}

// GroupsOf extracts the entries of the group described by spec. Unlike
// Groups, it knows the group hierarchy: a nested group is consumed as a block
// of exactly as many entries as its counter says, so tags it shares with the
// enclosing group cannot end an entry early. Entries are sub-slices of the
// message's fields.
func (m *Message) GroupsOf(spec *GroupSpec) []Group {
	return groupsOf(m.fields, spec)
}

// GroupsOf extracts the entries of a group nested in this entry; see
// Message.GroupsOf.
func (g *Group) GroupsOf(spec *GroupSpec) []Group {
	return groupsOf(g.fields, spec)
}

func groupsOf(fields []Field, spec *GroupSpec) []Group {
	for i := range fields {
		if fields[i].Tag != spec.Counter {
			continue
		}
		count, err := ParseInt(fields[i].Value)
		if err != nil || count <= 0 {
			return nil
		}
		groups, _ := spec.entries(fields, i+1, count)
		return groups
	}
	return nil
}

// entries reads up to count entries starting at fields[i] and returns them
// with the index just past the block.
func (s *GroupSpec) entries(fields []Field, i, count int) ([]Group, int) {
	var groups []Group
	for len(groups) < count && i < len(fields) && fields[i].Tag == s.Delim {
		start := i
		i = s.entry(fields, i)
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
func (s *GroupSpec) entry(fields []Field, i int) int {
	// Tags seen in this entry, as bits indexed by position in s.Tags, then
	// nested counters by position in s.Nested: on the stack for groups of up
	// to 256 of them.
	var small [4]uint64
	seen := small[:]
	if n := (len(s.Tags) + len(s.Nested) + 63) / 64; n > len(small) {
		seen = make([]uint64, n)
	}
	for first := true; i < len(fields); first = false {
		tag := fields[i].Tag
		if tag == s.Delim && !first {
			break // the next entry starts
		}
		if c := s.childIndex(tag); c >= 0 {
			// A nested counter, possibly the delimiter itself: it cannot
			// repeat within the entry either, then skip its entries by count.
			k := len(s.Tags) + c
			if seen[k/64]&(1<<(k%64)) != 0 {
				return i
			}
			seen[k/64] |= 1 << (k % 64)
			count, _ := ParseInt(fields[i].Value)
			i = s.Nested[c].skip(fields, i+1, count)
			continue
		}
		k := slices.Index(s.Tags, tag)
		switch {
		case k < 0 && !first:
			return i // not part of this group
		case k >= 0:
			if seen[k/64]&(1<<(k%64)) != 0 {
				return i // a tag cannot repeat within an entry
			}
			seen[k/64] |= 1 << (k % 64)
		}
		i++
	}
	return i
}

// skip returns the index just past up to count entries starting at
// fields[i], like entries without building them.
func (s *GroupSpec) skip(fields []Field, i, count int) int {
	for n := 0; n < count && i < len(fields) && fields[i].Tag == s.Delim; n++ {
		i = s.entry(fields, i)
	}
	return i
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
	for f := range m.TopLevelFields(specs...) {
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
	return func(yield func(Field) bool) {
		fields := m.fields
		for i := 0; i < len(fields); {
			f := fields[i]
			if !yield(f) {
				return
			}
			if spec := groupSpecFor(specs, f.Tag); spec != nil {
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
