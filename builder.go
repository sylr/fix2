package fix2

import (
	"fmt"
	"slices"
	"strconv"
)

// Builder constructs a FIX message from individual fields.
// Tags 8, 9, 35 and 10 are managed by Build. Setting or appending BeginString
// (8) or MsgType (35) replaces the value Build writes; setting BodyLength (9)
// or CheckSum (10), which Build computes, panics.
type Builder struct {
	beginString string
	msgType     string
	fields      []Field // body fields, values in arena
	arena       []byte  // owns the field values; reused by Reset
	scratch     []Field // group fields of the last serialization, reused
	onReset     func()  // clears the generated builder's state, see OnReset

	groups   func(dst []Field) []Field // appends repeating groups, see SetGroups
	counters []int                     // counter tags groups manages

	header       []int                     // standard header tags, see SetSections
	trailer      []int                     // standard trailer tags
	headerGroups func(dst []Field) []Field // appends the header's repeating groups
}

// SetSections registers the standard header and trailer: Build writes the
// fields whose tags are in header first, then the header's repeating groups
// (appended by headerGroups, which may be nil), then the other fields and
// groups, and the fields whose tags are in trailer last, as FIX requires
// whatever order they were set in. Counter tags of the header groups are
// registered with SetGroups. Generated message builders call it once, from
// their constructor.
func (b *Builder) SetSections(header, trailer []int, headerGroups func(dst []Field) []Field) {
	b.header = header
	b.trailer = trailer
	b.headerGroups = headerGroups
}

// SetGroups registers write, which appends the message's repeating groups to
// dst, and the counter tags it manages. Every Build and BuildWith call
// serializes the groups after the builder's own fields, so they are not lost
// whichever Build method a fluent chain ends on. A raw field whose tag is one
// of counters is left out when write emits that counter. Generated message
// builders call it once, from their constructor.
func (b *Builder) SetGroups(write func(dst []Field) []Field, counters ...int) {
	b.groups = write
	b.counters = counters
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
	b.set(tag, b.own(value))
	return b
}

// set sets tag to value, already owned, replacing an existing field.
func (b *Builder) set(tag int, value []byte) {
	for i := range b.fields {
		if b.fields[i].Tag == tag {
			b.fields[i].Value = value
			return
		}
	}
	b.fields = append(b.fields, Field{Tag: tag, Value: value})
}

// own copies value into the arena and returns the copy. A value replaced
// later stays in the arena until Reset. When the arena grows, earlier values
// keep pointing into the old one, which is never written again.
func (b *Builder) own(value []byte) []byte {
	start := len(b.arena)
	b.arena = append(b.arena, value...)
	return b.arena[start:len(b.arena):len(b.arena)]
}

// ownInt formats v into the arena and returns it.
func (b *Builder) ownInt(v int) []byte {
	start := len(b.arena)
	b.arena = strconv.AppendInt(b.arena, int64(v), 10)
	return b.arena[start:len(b.arena):len(b.arena)]
}

// ownFloat formats v into the arena, as AppendFloat does, and returns it.
func (b *Builder) ownFloat(v float64) []byte {
	start := len(b.arena)
	b.arena = AppendFloat(b.arena, v)
	return b.arena[start:len(b.arena):len(b.arena)]
}

// SetData sets a DATA field and the LENGTH field announcing it (see
// DataLengthTag), computing the length and keeping the two adjacent as FIX
// requires: any earlier value of either is removed and both are appended.
// value is copied.
func (b *Builder) SetData(lengthTag, dataTag int, value []byte) *Builder {
	for _, tag := range []int{lengthTag, dataTag} {
		switch tag {
		case tagBeginStr, tagBodyLen, tagMsgType, tagCheckSum:
			panic(fmt.Sprintf("fix2: Builder: tag %d is managed by Build and cannot be a DATA field", tag))
		}
	}
	b.fields = slices.DeleteFunc(b.fields, func(f Field) bool {
		return f.Tag == lengthTag || f.Tag == dataTag
	})
	b.fields = append(b.fields,
		Field{Tag: lengthTag, Value: b.ownInt(len(value))},
		Field{Tag: dataTag, Value: b.own(value)},
	)
	return b
}

// SetString sets a field to a string value.
func (b *Builder) SetString(tag int, value string) *Builder {
	if managedTag(tag) {
		return b.Set(tag, []byte(value))
	}
	start := len(b.arena)
	b.arena = append(b.arena, value...)
	b.set(tag, b.arena[start:len(b.arena):len(b.arena)])
	return b
}

// SetInt sets a field to an integer value.
func (b *Builder) SetInt(tag int, value int) *Builder {
	if managedTag(tag) {
		return b.Set(tag, strconv.AppendInt(nil, int64(value), 10))
	}
	b.set(tag, b.ownInt(value))
	return b
}

// SetFloat sets a field to a float64 value, in FIX float syntax: the shortest
// decimal that reads back as value, without exponent. It panics if value is
// NaN or infinite (see AppendFloat).
func (b *Builder) SetFloat(tag int, value float64) *Builder {
	if managedTag(tag) {
		return b.Set(tag, AppendFloat(nil, value))
	}
	b.set(tag, b.ownFloat(value))
	return b
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
	b.fields = append(b.fields, Field{Tag: tag, Value: b.own(value)})
	return b
}

// AppendString appends a string field.
func (b *Builder) AppendString(tag int, value string) *Builder {
	if managedTag(tag) {
		return b.Append(tag, []byte(value))
	}
	start := len(b.arena)
	b.arena = append(b.arena, value...)
	b.fields = append(b.fields, Field{Tag: tag, Value: b.arena[start:len(b.arena):len(b.arena)]})
	return b
}

// AppendInt appends an integer field.
func (b *Builder) AppendInt(tag int, value int) *Builder {
	if managedTag(tag) {
		return b.Append(tag, strconv.AppendInt(nil, int64(value), 10))
	}
	b.fields = append(b.fields, Field{Tag: tag, Value: b.ownInt(value)})
	return b
}

// AppendFloat appends a float64 field, formatted as SetFloat formats it. It
// panics if value is NaN or infinite.
func (b *Builder) AppendFloat(tag int, value float64) *Builder {
	if managedTag(tag) {
		return b.Append(tag, AppendFloat(nil, value))
	}
	b.fields = append(b.fields, Field{Tag: tag, Value: b.ownFloat(value)})
	return b
}

// AppendBool appends a FIX boolean field ("Y" or "N").
func (b *Builder) AppendBool(tag int, value bool) *Builder {
	if value {
		return b.Append(tag, []byte("Y"))
	}
	return b.Append(tag, []byte("N"))
}

// managedTag reports whether Build manages tag (see setManaged).
func managedTag(tag int) bool {
	switch tag {
	case tagBeginStr, tagMsgType, tagBodyLen, tagCheckSum:
		return true
	}
	return false
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

// MsgType returns the MsgType (tag 35) Build writes.
func (b *Builder) MsgType() string { return b.msgType }

// BeginString returns the BeginString (tag 8) Build writes.
func (b *Builder) BeginString() string { return b.beginString }

// Reset removes every field, keeping BeginString, MsgType, the registered
// sections and groups, and the storage, so that building message after
// message with one Builder stops allocating. It calls the function
// registered with OnReset, if any.
func (b *Builder) Reset() {
	clear(b.fields)
	b.fields = b.fields[:0]
	b.arena = b.arena[:0]
	clear(b.scratch)
	b.scratch = b.scratch[:0]
	if b.onReset != nil {
		b.onReset()
	}
}

// OnReset registers f, which Reset calls to clear what the builder keeps
// outside fix2.Builder, such as the entries of generated repeating groups.
func (b *Builder) OnReset(f func()) { b.onReset = f }

// Build serializes the message into a complete FIX wire-format byte slice:
//
//	8=<beginString>\x01 9=<bodyLen>\x01 35=<msgType>\x01 <fields>\x01 10=<checksum>\x01
func (b *Builder) Build() []byte {
	return b.BuildWith(nil)
}

// BuildWith is Build with extra fields serialized after the builder's own
// fields and groups, without adding them to the builder.
func (b *Builder) BuildWith(extra []Field) []byte {
	return b.appendMessage(nil, extra)
}

// AppendTo appends the message Build serializes to dst and returns the
// extended slice. It does not allocate when dst has room and the groups
// registered with SetGroups append without allocating.
func (b *Builder) AppendTo(dst []byte) []byte {
	return b.appendMessage(dst, nil)
}

// appendMessage appends the whole message to dst, with extra fields before
// the trailer.
func (b *Builder) appendMessage(dst []byte, extra []Field) []byte {
	start := len(dst)
	dst = append(dst, "8="...)
	dst = append(dst, b.beginString...)
	dst = append(dst, soh)
	dst = append(dst, "9="...)
	// BodyLength comes before the body: reserve its largest width, write
	// the body, then move the body back over the unused digits.
	lenPos := len(dst)
	dst = append(dst, "0000000000"...)
	dst = append(dst, soh)
	bodyStart := len(dst)
	dst = AppendTagValue(dst, tagMsgType, b.msgType)
	dst, _ = b.appendBody(dst, extra, nil)
	bodyLen := len(dst) - bodyStart
	var digits [20]byte
	n := strconv.AppendInt(digits[:0], int64(bodyLen), 10)
	shift := 10 - len(n)
	copy(dst[lenPos:], n)
	dst[lenPos+len(n)] = soh
	copy(dst[lenPos+len(n)+1:], dst[bodyStart:])
	dst = dst[:len(dst)-shift]
	return AppendTrailer(dst, start)
}

// AppendBody appends the fields Build writes after MsgType and before
// CheckSum: the standard header fields, the header's repeating groups, the
// other fields and groups, then the standard trailer fields (see
// SetSections). It is what a session layer sends, around its own header
// fields. A field of the builder whose tag reserved reports as true is an
// error: the session layer writes those itself. reserved may be nil.
func (b *Builder) AppendBody(dst []byte, reserved func(tag int) bool) ([]byte, error) {
	return b.appendBody(dst, nil, reserved)
}

// ReservedTagError reports a field AppendBody refused because the caller
// writes its tag itself.
type ReservedTagError struct {
	Tag int
}

func (e *ReservedTagError) Error() string {
	return fmt.Sprintf("fix2: tag %d is set by the session and cannot be set on the message", e.Tag)
}

// appendBody appends the body fields after MsgType, with extra fields before
// the trailer fields.
func (b *Builder) appendBody(dst []byte, extra []Field, reserved func(tag int) bool) ([]byte, error) {
	if reserved != nil {
		for _, f := range b.fields {
			if reserved(f.Tag) {
				return dst, &ReservedTagError{Tag: f.Tag}
			}
		}
	}
	// The header's groups, then the message's, in the scratch slice: the
	// first n fields are the header's.
	b.scratch = b.scratch[:0]
	if b.headerGroups != nil {
		b.scratch = b.headerGroups(b.scratch)
	}
	n := len(b.scratch)
	if b.groups != nil {
		b.scratch = b.groups(b.scratch)
	}
	const (
		header = iota
		body
		trailer
	)
	for _, section := range [...]int{header, body, trailer} {
		for _, f := range b.fields {
			// A raw counter is dropped when the groups carry their own.
			if b.section(f.Tag) == section && !b.managedCounter(f.Tag, b.scratch) {
				dst = appendField(dst, f)
			}
		}
		switch section {
		case header:
			for _, f := range b.scratch[:n] {
				dst = appendField(dst, f)
			}
		case body:
			for _, f := range b.scratch[n:] {
				dst = appendField(dst, f)
			}
			for _, f := range extra {
				dst = appendField(dst, f)
			}
		}
	}
	return dst, nil
}

// section returns where Build writes a field with tag: the header, the body
// or the trailer. A tag listed in both sections belongs to the header.
func (b *Builder) section(tag int) int {
	switch {
	case slices.Contains(b.header, tag):
		return 0
	case slices.Contains(b.trailer, tag):
		return 2
	}
	return 1
}

// appendField appends "tag=value\x01" to dst.
func appendField(dst []byte, f Field) []byte {
	dst = strconv.AppendInt(dst, int64(f.Tag), 10)
	dst = append(dst, '=')
	dst = append(dst, f.Value...)
	return append(dst, soh)
}

// AppendTagValue appends the field "tag=value\x01" to dst.
func AppendTagValue[V []byte | string](dst []byte, tag int, value V) []byte {
	dst = strconv.AppendInt(dst, int64(tag), 10)
	dst = append(dst, '=')
	dst = append(dst, value...)
	return append(dst, soh)
}

// AppendTagInt appends the field "tag=value\x01" to dst, value in decimal.
func AppendTagInt(dst []byte, tag int, value int64) []byte {
	dst = strconv.AppendInt(dst, int64(tag), 10)
	dst = append(dst, '=')
	dst = strconv.AppendInt(dst, value, 10)
	return append(dst, soh)
}

// CheckSum returns the FIX checksum of data: the sum of its bytes modulo 256.
func CheckSum(data []byte) byte {
	return byte(checksumBlock(data))
}

// AppendTrailer appends the CheckSum field "10=NNN\x01" of the message
// starting at dst[start] to dst.
func AppendTrailer(dst []byte, start int) []byte {
	sum := CheckSum(dst[start:])
	return append(dst, '1', '0', '=', '0'+sum/100, '0'+sum/10%10, '0'+sum%10, soh)
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

// managedCounter reports whether tag is a counter registered with SetGroups
// that groups, the serialized groups, contains.
func (b *Builder) managedCounter(tag int, groups []Field) bool {
	if !slices.Contains(b.counters, tag) {
		return false
	}
	for _, f := range groups {
		if f.Tag == tag {
			return true
		}
	}
	return false
}

// AppendField appends the field of fields with the given tag to dst, if
// there is one. Generated group entries call it for each tag of their
// layout, so fields are written in dictionary order.
func AppendField(dst, fields []Field, tag int) []Field {
	for _, f := range fields {
		if f.Tag == tag {
			return append(dst, f)
		}
	}
	return dst
}

// AppendCount appends the counter (NUMINGROUP) field of a repeating group
// with n entries.
func AppendCount(dst []Field, counter, n int) []Field {
	return append(dst, Field{Tag: counter, Value: strconv.AppendInt(nil, int64(n), 10)})
}
