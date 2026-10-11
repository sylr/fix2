package dictionary

import "strconv"

// Section is the part of a message a field belongs to.
type Section int

const (
	SectionBody Section = iota
	SectionHeader
	SectionTrailer
)

func (s Section) String() string {
	return [...]string{"body", "header", "trailer"}[s]
}

// SectionOf returns the part of d's messages tag belongs to: the standard
// header or trailer, their groups included, or else the body.
func (d *Dict) SectionOf(tag int) Section {
	return d.sections[tag]
}

// sectionsOf returns the header and trailer tags of d.
func (d *Dict) sectionsOf() map[int]Section {
	m := make(map[int]Section)
	var groups func(gs []GroupDef, s Section)
	groups = func(gs []GroupDef, s Section) {
		for _, g := range gs {
			m[g.CounterTag] = s
			for _, f := range g.Fields {
				m[f.Tag] = s
			}
			groups(g.Groups, s)
		}
	}
	for _, f := range d.Header {
		m[f.Tag] = SectionHeader
	}
	groups(d.HeaderGroups, SectionHeader)
	for _, f := range d.Trailer {
		m[f.Tag] = SectionTrailer
	}
	groups(d.TrailerGroups, SectionTrailer)
	return m
}

// AppendField appends a field as people read it: its tag, then its name,
// then its value, then the value's description, as in
// "35(MsgType)=D(NEW_ORDER_SINGLE)". Fields d does not define are appended
// as tag=value.
func (d *Dict) AppendField(dst []byte, tag int, value []byte) []byte {
	dst = strconv.AppendInt(dst, int64(tag), 10)
	fd := d.Fields[tag]
	if fd != nil {
		dst = append(dst, '(')
		dst = append(dst, fd.Name...)
		dst = append(dst, ')')
	}
	dst = append(dst, '=')
	dst = append(dst, value...)
	if fd != nil {
		if desc, ok := fd.Enums[string(value)]; ok {
			dst = append(dst, '(')
			dst = append(dst, desc...)
			dst = append(dst, ')')
		}
	}
	return dst
}
