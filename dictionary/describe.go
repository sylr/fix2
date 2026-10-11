package dictionary

import (
	"strconv"

	fix2 "github.com/sylr/fix2"
)

// Section is the part of a message a field belongs to.
type Section int

const (
	SectionHeader Section = iota
	SectionBody
	SectionTrailer
)

func (s Section) String() string {
	return [...]string{"header", "body", "trailer"}[s]
}

// Sections returns the part of m each of its fields belongs to, in the order
// of m.Fields(), as Validate places them: a field d declares only in the
// standard header or trailer, or that FIX requires there (BeginString,
// BodyLength, CheckSum), belongs to it; a field m's message type declares,
// to the body, even when the header or trailer declares it too; a group's
// entries, to its counter's part; and a field d does not know, to the body,
// or to the trailer after it.
func (d *Dict) Sections(m *fix2.Message) []Section {
	fields := m.Fields()
	out := make([]Section, len(fields))
	mdef := d.Messages[string(m.MsgType())]
	cur := SectionHeader
	for i := 0; i < len(fields); i++ {
		var e *tagEntry
		if mdef != nil {
			e = d.lookup(mdef, fields[i].Tag)
		} else {
			e = d.envelope.get(fields[i].Tag)
		}
		sec := max(cur, SectionBody)
		if e != nil {
			switch e.section {
			case sectionHeader:
				sec = SectionHeader
			case sectionTrailer:
				sec = SectionTrailer
			default:
				sec = SectionBody
			}
		}
		out[i] = sec
		if e != nil && e.spec != nil {
			next, _ := d.walkGroup(fields, i, e.spec, mdef, false)
			for j := i + 1; j < next; j++ {
				out[j] = sec
			}
			i = next - 1
		}
		cur = sec
	}
	return out
}

// AppendField appends a field as people read it: its tag, then its name,
// then its value, then the value's description, as in
// "35(MsgType)=D(NEW_ORDER_SINGLE)". Fields d does not define are appended
// as tag=value. Control characters and backslashes in the value are
// escaped, Go style: a value cannot start a line of its own.
func (d *Dict) AppendField(dst []byte, tag int, value []byte) []byte {
	dst = strconv.AppendInt(dst, int64(tag), 10)
	fd := d.Fields[tag]
	if fd != nil {
		dst = append(dst, '(')
		dst = append(dst, fd.Name...)
		dst = append(dst, ')')
	}
	dst = append(dst, '=')
	dst = appendEscaped(dst, value)
	if fd != nil {
		if desc, ok := fd.Enums[string(value)]; ok {
			dst = append(dst, '(')
			dst = append(dst, desc...)
			dst = append(dst, ')')
		}
	}
	return dst
}

// appendEscaped appends v with its control characters and backslashes
// escaped.
func appendEscaped(dst, v []byte) []byte {
	const hex = "0123456789abcdef"
	for _, c := range v {
		switch {
		case c == '\\':
			dst = append(dst, '\\', '\\')
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c < 0x20 || c == 0x7f:
			dst = append(dst, '\\', 'x', hex[c>>4], hex[c&0xf])
		default:
			dst = append(dst, c)
		}
	}
	return dst
}
