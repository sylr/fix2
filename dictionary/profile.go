package dictionary

import (
	"maps"
	"slices"

	fix2 "github.com/sylr/fix2"
)

// dataProfile returns the profile of d's DATA fields (types DATA and
// XMLDATA), each paired with the LENGTH field giving its size. Dictionaries
// do not record the pairs, so each DATA field takes, in order:
//   - the LENGTH field named after it with a "Len" or "Length" suffix
//     (RawData and RawDataLength, EncodedText and EncodedTextLen);
//   - the LENGTH field the standard table pairs it with (see
//     fix2.DefaultProfile), for abbreviated names, even if d does not
//     define it: the parser requires it, and generated builders write it;
//   - the LENGTH field right before it in a message, group, header or
//     trailer.
//
// A LENGTH field pairs with one DATA field at most; a DATA field left without
// one is parsed up to the next SOH.
func (d *Dict) dataProfile() *fix2.Profile {
	isType := func(tag int, types ...string) bool {
		fd := d.Fields[tag]
		return fd != nil && slices.Contains(types, fd.Type)
	}
	isData := func(tag int) bool { return isType(tag, "DATA", "XMLDATA") }
	isLength := func(tag int) bool { return tag != 9 && isType(tag, "LENGTH") }

	var pairs []fix2.DataPair
	used := map[int]bool{}
	pair := func(length, data int, undeclared bool) bool {
		if used[length] || used[data] || !(isLength(length) || undeclared && d.Fields[length] == nil) {
			return false
		}
		used[length], used[data] = true, true
		pairs = append(pairs, fix2.DataPair{Length: length, Data: data})
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
			if l := fix2.DefaultProfile().DataLengthTag(tag); l != 0 {
				pair(l, tag, true)
			}
		}
	}
	// By position, within one scope: a message's fields hold its groups'
	// fields too, which are another scope.
	walkRefs := func(refs []FieldRef, skip map[int]bool) {
		for i := 1; i < len(refs); i++ {
			l, t := refs[i-1].Tag, refs[i].Tag
			if !used[t] && isData(t) && !skip[l] && !skip[t] {
				pair(l, t, false)
			}
		}
	}
	// groupTags adds the tags of groups, nested ones included, to tags, and
	// walks each group's own fields, without its nested groups' fields.
	var walkGroups func(groups []GroupDef, tags map[int]bool)
	walkGroups = func(groups []GroupDef, tags map[int]bool) {
		for _, g := range groups {
			nested := map[int]bool{}
			walkGroups(g.Groups, nested)
			walkRefs(g.Fields, nested)
			tags[g.CounterTag] = true
			for _, f := range g.Fields {
				tags[f.Tag] = true
			}
			for t := range nested {
				tags[t] = true
			}
		}
	}
	for _, scope := range []struct {
		refs   []FieldRef
		groups []GroupDef
	}{{d.Header, d.HeaderGroups}, {d.Trailer, d.TrailerGroups}} {
		inGroups := map[int]bool{}
		walkGroups(scope.groups, inGroups)
		walkRefs(scope.refs, inGroups)
	}
	for _, mt := range slices.Sorted(maps.Keys(d.Messages)) {
		inGroups := map[int]bool{}
		walkGroups(d.Messages[mt].Groups, inGroups)
		walkRefs(d.Messages[mt].Fields, inGroups)
	}

	p, err := fix2.NewProfile(pairs)
	if err != nil {
		// Tags out of the profile's range: parse those fields as plain.
		pairs = slices.DeleteFunc(pairs, func(p fix2.DataPair) bool {
			_, err := fix2.NewProfile([]fix2.DataPair{p})
			return err != nil
		})
		p, _ = fix2.NewProfile(pairs)
	}
	return p
}
