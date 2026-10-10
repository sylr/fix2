package dictionary

import fix2 "github.com/sylr/fix2"

// Internals the external tests inspect.

const MaxSharedSpecs = maxSharedSpecs

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

// Specs returns the group specs m skips for top-level presence.
func (m *MessageDef) Specs() []*fix2.GroupSpec { return m.specs }

// EnumChecks returns how many enum checks d's tables hold.
func (d *Dict) EnumChecks() int { return len(d.enumChecks) }

// EnvelopeSpecs returns the envelope specs d keeps apart from its messages'.
func (d *Dict) EnvelopeSpecs() []*fix2.GroupSpec { return d.envelopeSpecs }

// Allows reports whether m's compiled body table holds tag.
func (m *MessageDef) Allows(tag int) bool { return m.body.get(tag) != nil }
