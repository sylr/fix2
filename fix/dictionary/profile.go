package dictionary

import (
	"maps"
	"slices"

	"github.com/sylr/fix2/fix"
)

// dataProfile returns the profile of d's DATA fields (types DATA and
// XMLDATA), each paired with the LENGTH field giving its size. Dictionaries
// do not record the pairs, so each DATA field takes, in order:
//   - the LENGTH field named after it with a "Len" or "Length" suffix
//     (RawData and RawDataLength, EncodedText and EncodedTextLen);
//   - the LENGTH field the standard table pairs it with (see
//     fix.DefaultProfile), for abbreviated names, even if d does not
//     define it: the parser requires it, and generated builders write it;
//   - the LENGTH field right before it in a message, group, header or
//     trailer.
//
// A LENGTH field pairs with one DATA field at most; a DATA field left without
// one is parsed up to the next SOH.
func (d *Dict) dataProfile() *fix.Profile {
	isType := func(tag int, types ...string) bool {
		fd := d.Fields[tag]
		return fd != nil && slices.Contains(types, fd.Type)
	}
	isData := func(tag int) bool { return isType(tag, "DATA", "XMLDATA") }
	isLength := func(tag int) bool { return tag != 9 && isType(tag, "LENGTH") }

	var pairs []fix.DataPair
	used := map[int]bool{}
	pair := func(length, data int, undeclared bool) bool {
		if used[length] || used[data] || !(isLength(length) || undeclared && d.Fields[length] == nil) {
			return false
		}
		used[length], used[data] = true, true
		pairs = append(pairs, fix.DataPair{Length: length, Data: data})
		return true
	}
	datas := slices.Sorted(maps.Keys(d.Fields))
	datas = slices.DeleteFunc(datas, func(tag int) bool { return !isData(tag) })

	// By name.
	for _, tag := range datas {
		name := d.Fields[tag].Name
		for _, suffix := range []string{"Len", "Length"} {
			if fd := d.fieldsByName[name+suffix]; fd != nil && pair(fd.Tag, tag, false) {
				break
			}
		}
	}
	// By the standard table.
	for _, tag := range datas {
		if !used[tag] {
			if l := fix.DefaultProfile().DataLengthTag(tag); l != 0 {
				pair(l, tag, true)
			}
		}
	}
	// By position, within one scope. A group's fields are its own scope;
	// the flattened fields of a message (or of the header or trailer) hold
	// its groups' fields too, so a pair there counts only if its LENGTH
	// field occurs at the top level (more often than in the groups) and its
	// DATA field only there.
	walkRefs := func(refs []FieldRef, inGroups map[int]int) {
		counts := map[int]int{}
		for _, r := range refs {
			counts[r.Tag]++
		}
		top := func(tag int) bool { return counts[tag] > inGroups[tag] }
		for i := 1; i < len(refs); i++ {
			l, t := refs[i-1].Tag, refs[i].Tag
			if !used[t] && isData(t) && inGroups[t] == 0 && top(l) {
				pair(l, t, false)
			}
		}
	}
	// walkGroups walks each group's own fields and counts the tags of
	// groups, nested ones included, into tags.
	var walkGroups func(groups []GroupDef, tags map[int]int)
	walkGroups = func(groups []GroupDef, tags map[int]int) {
		for _, g := range groups {
			nested := map[int]int{}
			walkGroups(g.Groups, nested)
			walkRefs(g.Fields, nested)
			tags[g.CounterTag]++
			for _, f := range g.Fields {
				tags[f.Tag]++
			}
			for t, n := range nested {
				tags[t] += n
			}
		}
	}
	for _, scope := range []struct {
		refs   []FieldRef
		groups []GroupDef
	}{{d.Header, d.HeaderGroups}, {d.Trailer, d.TrailerGroups}} {
		inGroups := map[int]int{}
		walkGroups(scope.groups, inGroups)
		walkRefs(scope.refs, inGroups)
	}
	for _, mt := range slices.Sorted(maps.Keys(d.Messages)) {
		inGroups := map[int]int{}
		walkGroups(d.Messages[mt].Groups, inGroups)
		walkRefs(d.Messages[mt].Fields, inGroups)
	}

	// Tags out of the profile's range: parse those fields as plain. Testing
	// each pair with NewProfile would build a table per pair, as large as its
	// highest tag.
	pairs = slices.DeleteFunc(pairs, func(p fix.DataPair) bool { return !p.InRange() })
	// Pairs share no tag and a pair's tags differ, so NewProfile cannot fail
	// here. A nil profile would parse as the default one, silently changing
	// which fields are DATA.
	p, err := fix.NewProfile(pairs)
	if err != nil {
		panic(err)
	}
	return p
}
