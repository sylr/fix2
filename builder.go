package fix2

import (
	"fmt"
	"strconv"
)

// Builder constructs a FIX message from individual fields.
// Tags 8, 9, 35 and 10 are managed by Build. Setting or appending BeginString
// (8) or MsgType (35) replaces the value Build writes; setting BodyLength (9)
// or CheckSum (10), which Build computes, panics.
type Builder struct {
	beginString string
	msgType     string
	fields      []Field // body fields (owned []byte, not zero-copy)
}

// NewBuilder creates a Builder for the given BeginString and MsgType.
func NewBuilder(beginString, msgType string) *Builder {
	return &Builder{
		beginString: beginString,
		msgType:     msgType,
	}
}

// Set sets a body/header field to a raw byte value.
// If the tag already exists it is replaced; otherwise it is appended.
func (b *Builder) Set(tag int, value []byte) *Builder {
	if b.setManaged(tag, value) {
		return b
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	for i := range b.fields {
		if b.fields[i].Tag == tag {
			b.fields[i].Value = cp
			return b
		}
	}
	b.fields = append(b.fields, Field{Tag: tag, Value: cp})
	return b
}

// SetString sets a field to a string value.
func (b *Builder) SetString(tag int, value string) *Builder {
	return b.Set(tag, []byte(value))
}

// SetInt sets a field to an integer value.
func (b *Builder) SetInt(tag int, value int) *Builder {
	return b.Set(tag, strconv.AppendInt(nil, int64(value), 10))
}

// SetFloat sets a field to a float64 value.
// The value is formatted with up to 10 significant digits and no trailing zeros.
func (b *Builder) SetFloat(tag int, value float64) *Builder {
	return b.Set(tag, strconv.AppendFloat(nil, value, 'f', -1, 64))
}

// SetBool sets a field to a FIX boolean value ("Y" or "N").
func (b *Builder) SetBool(tag int, value bool) *Builder {
	if value {
		return b.Set(tag, []byte("Y"))
	}
	return b.Set(tag, []byte("N"))
}

// Append appends a field without checking for duplicates.
// This is needed for repeating groups where the same tag appears multiple times.
func (b *Builder) Append(tag int, value []byte) *Builder {
	if b.setManaged(tag, value) {
		return b
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	b.fields = append(b.fields, Field{Tag: tag, Value: cp})
	return b
}

// AppendString appends a string field.
func (b *Builder) AppendString(tag int, value string) *Builder {
	return b.Append(tag, []byte(value))
}

// AppendInt appends an integer field.
func (b *Builder) AppendInt(tag int, value int) *Builder {
	return b.Append(tag, strconv.AppendInt(nil, int64(value), 10))
}

// AppendFloat appends a float64 field.
func (b *Builder) AppendFloat(tag int, value float64) *Builder {
	return b.Append(tag, strconv.AppendFloat(nil, value, 'f', -1, 64))
}

// AppendBool appends a FIX boolean field ("Y" or "N").
func (b *Builder) AppendBool(tag int, value bool) *Builder {
	if value {
		return b.Append(tag, []byte("Y"))
	}
	return b.Append(tag, []byte("N"))
}

// setManaged handles the tags Build manages. It reports whether tag was one
// of them, so the caller must not store it as a body field.
func (b *Builder) setManaged(tag int, value []byte) bool {
	switch tag {
	case tagBeginStr:
		b.beginString = string(value)
		return true
	case tagMsgType:
		b.msgType = string(value)
		return true
	case tagBodyLen, tagCheckSum:
		panic(fmt.Sprintf("fix2: Builder: tag %d is computed by Build and cannot be set", tag))
	}
	return false
}

// Build serializes the message into a complete FIX wire-format byte slice:
//
//	8=<beginString>\x01 9=<bodyLen>\x01 35=<msgType>\x01 <fields>\x01 10=<checksum>\x01
func (b *Builder) Build() []byte {
	return b.BuildWith(nil)
}

// BuildWith is Build with extra fields serialized after the builder's own,
// without adding them to the builder. Generated builders use it to write
// their repeating groups.
func (b *Builder) BuildWith(extra []Field) []byte {
	// Build body: 35=<msgType>\x01 + all user fields.
	var body []byte
	body = append(body, "35="...)
	body = append(body, b.msgType...)
	body = append(body, soh)
	for _, f := range b.fields {
		body = strconv.AppendInt(body, int64(f.Tag), 10)
		body = append(body, '=')
		body = append(body, f.Value...)
		body = append(body, soh)
	}
	for _, f := range extra {
		body = strconv.AppendInt(body, int64(f.Tag), 10)
		body = append(body, '=')
		body = append(body, f.Value...)
		body = append(body, soh)
	}

	// Header: 8=<beginString>\x01 9=<bodyLen>\x01
	var msg []byte
	msg = append(msg, "8="...)
	msg = append(msg, b.beginString...)
	msg = append(msg, soh)
	msg = append(msg, "9="...)
	msg = strconv.AppendInt(msg, int64(len(body)), 10)
	msg = append(msg, soh)
	msg = append(msg, body...)

	// Checksum: sum of all bytes mod 256, formatted as 3-digit string.
	var csum uint32
	for _, c := range msg {
		csum += uint32(c)
	}
	msg = fmt.Appendf(msg, "10=%03d\x01", csum%256)
	return msg
}

// SetField sets tag to value in fields: an existing field with that tag is
// replaced in place, otherwise one is appended. value is stored as is.
// Generated group entry builders use it so that setting a field twice keeps
// a single value.
func SetField(fields []Field, tag int, value []byte) []Field {
	for i := range fields {
		if fields[i].Tag == tag {
			fields[i].Value = value
			return fields
		}
	}
	return append(fields, Field{Tag: tag, Value: value})
}

// AppendEntry appends a repeating group entry's own fields to dst: the field
// with tag delim first, as FIX requires, then the others in their original
// order.
func AppendEntry(dst, fields []Field, delim int) []Field {
	for _, f := range fields {
		if f.Tag == delim {
			dst = append(dst, f)
			break
		}
	}
	for _, f := range fields {
		if f.Tag != delim {
			dst = append(dst, f)
		}
	}
	return dst
}

// AppendCount appends the counter (NUMINGROUP) field of a repeating group
// with n entries.
func AppendCount(dst []Field, counter, n int) []Field {
	return append(dst, Field{Tag: counter, Value: strconv.AppendInt(nil, int64(n), 10)})
}
