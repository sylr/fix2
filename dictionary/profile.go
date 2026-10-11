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
	// By position.
	var walkRefs func(refs []FieldRef)
	walkRefs = func(refs []FieldRef) {
		for i := 1; i < len(refs); i++ {
			if t := refs[i].Tag; !used[t] && isData(t) {
				pair(refs[i-1].Tag, t, false)
			}
		}
	}
	var walkGroups func(groups []GroupDef)
	walkGroups = func(groups []GroupDef) {
		for _, g := range groups {
			walkRefs(g.Fields)
			walkGroups(g.Groups)
		}
	}
	walkRefs(d.Header)
	walkRefs(d.Trailer)
	walkGroups(d.HeaderGroups)
	walkGroups(d.TrailerGroups)
	for _, mt := range slices.Sorted(maps.Keys(d.Messages)) {
		walkRefs(d.Messages[mt].Fields)
		walkGroups(d.Messages[mt].Groups)
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
