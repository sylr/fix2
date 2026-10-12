package dictionary

import fix2 "github.com/sylr/fix2"

// Internals the external tests inspect.

// RequiredTags returns the tags m requires unconditionally, in order.
func (m *MessageDef) RequiredTags() []int {
	var tags []int
	for _, t := range m.requiredTags {
		tags = append(tags, t.tag)
	}
	return tags
}

// CompiledConds returns how many conditions m's tables hold.
func (m *MessageDef) CompiledConds() int { return len(m.conds) }

// CounterSpec returns the spec of the group whose counter is tag, as m's
// tables give it to top-level presence, or nil.
func (d *Dict) CounterSpec(m *MessageDef, tag int) *fix2.GroupSpec {
	if e := d.lookup(m, tag); e != nil {
		return e.spec
	}
	return nil
}

// EnumChecks returns how many enum checks d's tables hold.
func (d *Dict) EnumChecks() int { return len(d.enumChecks) }

// Allows reports whether m's compiled body table holds tag.
func (m *MessageDef) Allows(tag int) bool { return m.body.get(tag) != nil }

// TopLevel reports whether m allows tag outside group entries.
func (d *Dict) TopLevel(m *MessageDef, tag int) bool {
	e := d.lookup(m, tag)
	return e != nil && !e.grouped
}
