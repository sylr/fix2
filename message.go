package fix2

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

// BeginString returns the value of tag 8.
func (m *Message) BeginString() []byte { return m.Get(8) }

// BodyLength returns the value of tag 9.
func (m *Message) BodyLength() []byte { return m.Get(9) }

// MsgType returns the value of tag 35.
func (m *Message) MsgType() []byte { return m.Get(35) }
