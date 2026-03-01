package fix2

import "slices"

// Message represents a parsed FIX message.
// The inline field buffer avoids heap allocation for messages with up to 32 fields.
type Message struct {
	buf    [32]Field
	fields []Field
}

func (m *Message) init() {
	m.fields = m.buf[:0]
}

func (m *Message) append(f Field) {
	m.fields = append(m.fields, f)
}

// Fields returns the parsed fields. The returned slice must not be modified.
func (m *Message) Fields() []Field {
	return m.fields
}

// Count returns the number of parsed fields.
func (m *Message) Count() int {
	return len(m.fields)
}

// Get returns the value of the first field with the given tag, or nil if not found.
func (m *Message) Get(tag int) []byte {
	for i := range m.fields {
		if m.fields[i].Tag == tag {
			return m.fields[i].Value
		}
	}
	return nil
}

// Groups extracts repeating group instances from the message.
// counterTag is the NUMINGROUP tag and childTags lists all tags belonging to
// the group. childTags[0] is the delimiter tag that starts each instance.
// Scanning stops when a non-child tag is encountered or count instances have
// been collected.
func (m *Message) Groups(counterTag int, childTags []int) []Group {
	if len(childTags) == 0 {
		return nil
	}
	delimTag := childTags[0]

	// 1. Find the counter field and parse its value.
	counterIdx := -1
	count := 0
	for i := range m.fields {
		if m.fields[i].Tag == counterTag {
			counterIdx = i
			for _, c := range m.fields[i].Value {
				count = count*10 + int(c-'0')
			}
			break
		}
	}
	if counterIdx < 0 || count == 0 {
		return nil
	}

	// 2. Scan forward from the field after the counter.
	var groups []Group
	var cur []Field
	for i := counterIdx + 1; i < len(m.fields) && len(groups) < count; i++ {
		f := m.fields[i]
		if !slices.Contains(childTags, f.Tag) {
			break // non-child tag terminates the group block
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

// BeginString returns the value of tag 8.
func (m *Message) BeginString() []byte { return m.Get(8) }

// BodyLength returns the value of tag 9.
func (m *Message) BodyLength() []byte { return m.Get(9) }

// MsgType returns the value of tag 35.
func (m *Message) MsgType() []byte { return m.Get(35) }
