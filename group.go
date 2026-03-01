package fix2

import "slices"

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
			for _, c := range g.fields[i].Value {
				count = count*10 + int(c-'0')
			}
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
