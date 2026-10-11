package dictionary

import (
	"maps"
	"math/bits"
	"math/rand/v2"
	"slices"

	fix2 "github.com/sylr/fix2"
)

// Validate runs on tables compiled once by New, so that validating a message
// neither builds maps nor allocates:
//
//   - tagTable lookups tell whether a tag is allowed and which presence slot,
//     if any, records it. The header and trailer have one table, shared by
//     all messages, and each message one for its body.
//   - Required tags and the tags activating optional components each have a
//     slot; presence anywhere and at the top level are two bitsets.
//   - Enum values are checked against a 256-bit set of single-byte values,
//     falling back to the dictionary's map for longer ones.

// tagTable maps tags to entries with open addressing and linear probing.
// Tags are hashed by multiply-shift with a random odd multiplier per table,
// which a dictionary cannot know: it cannot be crafted to make its tags
// collide and building its tables quadratic. Multiply-shift is not enough
// for constant expected probes on every key set, but structured sets cost at
// most logarithmically many, and tables stay at most half full.
type tagTable struct {
	entries []tagEntry
	shift   uint
	mul     uint64
}

// tagEntry is what a scope (the envelope or a message body) knows of a tag.
type tagEntry struct {
	tag        int
	slot       int32           // presence slot, or -1 when the tag's presence is not tracked
	counter    bool            // counter of a group of the message, header or trailer
	spec       *fix2.GroupSpec // that group's spec, whose entries top-level presence skips
	structural bool            // BeginString, BodyLength or CheckSum, never enum-checked
	declared   bool            // declared by the scope, not only a DATA/LENGTH partner
	section    section         // where FIX requires the field: header, body or trailer
	used       bool
}

// section is where FIX requires a field: the standard header first, the body,
// then the standard trailer.
type section uint8

const (
	sectionHeader section = iota
	sectionBody
	sectionTrailer
	// sectionAny is a body field the header or trailer declares too: it
	// may be in either place.
	sectionAny
)

// newTagTable returns a table holding entries, keyed by their tags.
func newTagTable(entries map[int]tagEntry) tagTable {
	size := 8
	for size < 2*len(entries) {
		size <<= 1
	}
	t := tagTable{entries: make([]tagEntry, size), shift: uint(64 - bits.TrailingZeros(uint(size))), mul: rand.Uint64() | 1}
	for tag, e := range entries {
		e.tag, e.used = tag, true
		i := t.index(tag)
		for t.entries[i].used {
			i = (i + 1) & (size - 1)
		}
		t.entries[i] = e
	}
	return t
}

// index returns the slot where looking tag up starts.
func (t *tagTable) index(tag int) int {
	return int(uint64(tag) * t.mul >> t.shift)
}

// get returns the entry for tag, or nil.
func (t *tagTable) get(tag int) *tagEntry {
	mask := len(t.entries) - 1
	for i := t.index(tag); t.entries[i].used; i = (i + 1) & mask {
		if t.entries[i].tag == tag {
			return &t.entries[i]
		}
	}
	return nil
}

// tracked is a tag whose presence a slot records.
type tracked struct {
	tag  int
	slot int32
}

// compiledCond is a condition with its tags resolved to presence slots.
type compiledCond struct {
	component  string
	activation []tracked // tags showing the component is present
	required   []tracked
}

// enumCheck holds the valid values of a field with enums.
type enumCheck struct {
	name   string
	multi  bool              // space-separated values (see isMultipleValue)
	chars  [4]uint64         // single-byte values
	values map[string]string // all values, consulted for longer ones; nil if none
}

// valid reports whether v is one of the field's values.
func (e *enumCheck) valid(v []byte) bool {
	if len(v) == 1 {
		return e.chars[v[0]>>6]&(1<<(v[0]&63)) != 0
	}
	if e.values == nil {
		return false
	}
	_, ok := e.values[string(v)]
	return ok
}

// slots assigns presence slots to tags, numbered from a base, in the order
// tags are first tracked.
type slots struct {
	base int32
	of   map[int]int32
	n    int32
}

func (s *slots) track(tag int) int32 {
	if slot, ok := s.of[tag]; ok {
		return slot
	}
	slot := s.base + s.n
	s.of[tag] = slot
	s.n++
	return slot
}

// compile builds the tables Validate uses from d's exported fields, replacing
// any built before.
func (d *Dict) compile() {
	d.enumChecks = nil
	for _, m := range d.Messages {
		m.requiredTags, m.conds = nil, nil
	}
	// Enum checks, by tag.
	enumIdx := map[int]tagEntry{}
	for _, tag := range slices.Sorted(maps.Keys(d.Fields)) {
		fd := d.Fields[tag]
		if len(fd.Enums) == 0 {
			continue
		}
		e := enumCheck{name: fd.Name, multi: isMultipleValue(fd.Type)}
		long := false
		for v := range fd.Enums {
			if len(v) == 1 {
				e.chars[v[0]>>6] |= 1 << (v[0] & 63)
			} else {
				long = true
			}
		}
		if long {
			e.values = maps.Clone(fd.Enums)
		}
		enumIdx[tag] = tagEntry{slot: int32(len(d.enumChecks))}
		d.enumChecks = append(d.enumChecks, e)
	}
	d.enums = newTagTable(enumIdx)
	d.compileFormats()

	// Envelope: slots for its required and condition tags, shared by all
	// messages, then the table of the tags it allows.
	env := slots{of: map[int]int32{}}
	d.headerReq = requiredOf(d.Header, &env)
	d.trailerReq = requiredOf(d.Trailer, &env)
	d.headerConds = compileConds(d.HeaderConditions, &env)
	d.trailerConds = compileConds(d.TrailerConditions, &env)
	d.envSlots = env.n

	envCounters := specsByCounter(slices.Concat(d.groupSpecs(d.HeaderGroups), d.groupSpecs(d.TrailerGroups)))
	envEntries := map[int]tagEntry{}
	addAllowed(d.profile, envEntries, d.Header, sectionHeader, env.of, nil, envCounters, nil)
	addAllowed(d.profile, envEntries, d.Trailer, sectionTrailer, env.of, nil, envCounters, nil)
	for tag, s := range map[int]section{8: sectionHeader, 9: sectionHeader, 10: sectionTrailer} {
		e, ok := envEntries[tag]
		if !ok {
			e = tagEntry{slot: -1, structural: true}
		}
		e.section = s
		envEntries[tag] = e
	}
	d.envelope = newTagTable(envEntries)

	// Each message: its own slots, after the envelope's, and its table. A tag
	// the envelope tracks keeps the envelope's slot, so that one lookup in
	// the body table records it for both.
	for _, m := range d.Messages {
		body := slots{base: env.n, of: map[int]int32{}}
		shared := func(tag int) int32 {
			if slot, ok := env.of[tag]; ok {
				return slot
			}
			return body.track(tag)
		}
		seen := map[int]bool{}
		for _, ref := range m.Fields {
			if ref.Required && !seen[ref.Tag] {
				seen[ref.Tag] = true
				m.requiredTags = append(m.requiredTags, tracked{tag: ref.Tag, slot: shared(ref.Tag)})
			}
		}
		for _, c := range m.Conditions {
			cc := compiledCond{component: c.Component}
			for _, tag := range c.Activation {
				cc.activation = append(cc.activation, tracked{tag: tag, slot: shared(tag)})
			}
			for _, tag := range c.Required {
				cc.required = append(cc.required, tracked{tag: tag, slot: shared(tag)})
			}
			m.conds = append(m.conds, cc)
		}
		m.slots = env.n + body.n

		entries := map[int]tagEntry{}
		addAllowed(d.profile, entries, m.Fields, sectionBody, body.of, env.of, specsByCounter(d.groupSpecs(m.Groups)), envCounters)
		for tag, e := range entries {
			if env, ok := envEntries[tag]; ok && !env.structural {
				e.section = sectionAny
				entries[tag] = e
			}
		}
		m.body = newTagTable(entries)
	}
}

// requiredOf returns the required tags of refs, in order, with their slots.
func requiredOf(refs []FieldRef, s *slots) []tracked {
	var out []tracked
	seen := map[int]bool{}
	for _, ref := range refs {
		if ref.Required && !seen[ref.Tag] {
			seen[ref.Tag] = true
			out = append(out, tracked{tag: ref.Tag, slot: s.track(ref.Tag)})
		}
	}
	return out
}

// compileConds resolves the tags of conds to slots.
func compileConds(conds []Condition, s *slots) []compiledCond {
	var out []compiledCond
	for _, c := range conds {
		cc := compiledCond{component: c.Component}
		for _, tag := range c.Activation {
			cc.activation = append(cc.activation, tracked{tag: tag, slot: s.track(tag)})
		}
		for _, tag := range c.Required {
			cc.required = append(cc.required, tracked{tag: tag, slot: s.track(tag)})
		}
		out = append(out, cc)
	}
	return out
}

// addAllowed adds the tags of refs to entries, with the LENGTH or DATA field
// each pairs with: the parser requires LENGTH right before its DATA field,
// and generated builders write both, even when the dictionary declares only
// one. A tag's slot comes from own, else from shared, else it has none; it
// is a group counter if sharedCounters, the envelope's, or counters has its
// spec, in that order.
func addAllowed(profile *fix2.Profile, entries map[int]tagEntry, refs []FieldRef, sec section, own, shared map[int]int32, counters, sharedCounters map[int]*fix2.GroupSpec) {
	add := func(tag int) {
		if _, ok := entries[tag]; ok {
			return
		}
		e := tagEntry{slot: -1, structural: structural(tag), section: sec}
		if e.spec = sharedCounters[tag]; e.spec == nil {
			e.spec = counters[tag]
		}
		e.counter = e.spec != nil
		if slot, ok := own[tag]; ok {
			e.slot = slot
		} else if slot, ok := shared[tag]; ok {
			e.slot = slot
		}
		entries[tag] = e
	}
	for _, ref := range refs {
		add(ref.Tag)
		e := entries[ref.Tag]
		e.declared = true
		entries[ref.Tag] = e
	}
	// Partners after declared tags, so a declared tag keeps its own entry.
	for _, ref := range refs {
		for _, p := range []int{profile.DataTag(ref.Tag), profile.DataLengthTag(ref.Tag)} {
			if p != 0 {
				add(p)
			}
		}
	}
	// Every tracked tag is allowed: required and activation tags come from
	// the scope's own fields, but make sure of it.
	for tag := range own {
		add(tag)
	}
}

// structural reports whether tag is BeginString, BodyLength or CheckSum, which
// the parser manages: always allowed and never enum-checked.
func structural(tag int) bool { return tag == 8 || tag == 9 || tag == 10 }

// specsByCounter indexes specs by counter, the first one winning.
func specsByCounter(specs []*fix2.GroupSpec) map[int]*fix2.GroupSpec {
	m := make(map[int]*fix2.GroupSpec, len(specs))
	for _, s := range specs {
		if m[s.Counter] == nil {
			m[s.Counter] = s
		}
	}
	return m
}
