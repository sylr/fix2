package dictionary

import (
	"fmt"

	fix2 "github.com/sylr/fix2"
)

// Checks selects the validations ValidateWith adds to Validate's. A session
// enables them all by default, as QuickFIX does.
type Checks uint

const (
	// CheckEmptyValues rejects fields with an empty value (reason 4).
	CheckEmptyValues Checks = 1 << iota
	// CheckFormats rejects values not in their field type's format: integers,
	// floats, single characters, booleans, timestamps, times, dates and
	// month-years (reason 6).
	CheckFormats
	// CheckRepeatedTags rejects a tag appearing more than once outside group
	// entries (reason 13).
	CheckRepeatedTags
	// CheckOrder rejects standard header fields after the body, and body
	// fields after the standard trailer (reason 14).
	CheckOrder
	// CheckGroupCounts rejects repeating groups whose NumInGroup does not
	// match their entries (reason 16).
	CheckGroupCounts

	// CheckAll enables every check.
	CheckAll = CheckEmptyValues | CheckFormats | CheckRepeatedTags | CheckOrder | CheckGroupCounts
)

// ValidateWith checks msg as Validate does, plus the checks selected. Field
// errors are reported in message order, after missing required tags; repeated
// tags, order and group counts after them. Unlike Validate, the extra checks
// may allocate.
func (d *Dict) ValidateWith(msg *fix2.Message, checks Checks) error {
	return d.validate(msg, checks)
}

// format is the syntax a field type requires of its values.
type format int32

const (
	formatNone format = iota
	formatInt
	formatUint
	formatFloat
	formatChar
	formatBool
	formatTimestamp
	formatTimeOnly
	formatDate
	formatMonthYear
)

// formatOf returns the format of values of fixType in a dictionary of FIX
// version major.minor: CHAR was any string before FIX 4.2.
func formatOf(fixType, major, minor string) format {
	switch fixType {
	case "INT", "DAYOFMONTH":
		return formatInt
	case "LENGTH", "NUMINGROUP", "SEQNUM", "TAGNUM":
		return formatUint
	case "FLOAT", "QTY", "PRICE", "PRICEOFFSET", "AMT", "PERCENTAGE":
		return formatFloat
	case "CHAR":
		if major == "4" && (minor == "0" || minor == "1") {
			return formatNone
		}
		return formatChar
	case "BOOLEAN":
		return formatBool
	case "UTCTIMESTAMP", "TIME":
		return formatTimestamp
	case "UTCTIMEONLY":
		return formatTimeOnly
	case "UTCDATEONLY", "UTCDATE", "LOCALMKTDATE", "DATE":
		return formatDate
	case "MONTHYEAR":
		return formatMonthYear
	}
	return formatNone
}

// compileFormats builds the table of the formats of d's fields, by tag.
func (d *Dict) compileFormats() {
	formats := map[int]tagEntry{}
	for tag, fd := range d.Fields {
		if f := formatOf(fd.Type, d.Major, d.Minor); f != formatNone {
			formats[tag] = tagEntry{slot: int32(f)}
		}
	}
	d.formats = newTagTable(formats)
}

// checkValue returns an error if f's value is empty or not in its field
// type's format, as checks select.
func (d *Dict) checkValue(f fix2.Field, checks Checks) error {
	if len(f.Value) == 0 {
		if checks&CheckEmptyValues == 0 {
			return nil
		}
		return &ValidationError{
			Err:    ErrEmptyValue,
			Tag:    f.Tag,
			Reason: ReasonTagSpecifiedWithoutAValue,
			Msg:    fmt.Sprintf("tag %d%s has no value", f.Tag, d.fieldName(f.Tag)),
		}
	}
	if checks&CheckFormats == 0 {
		return nil
	}
	e := d.formats.get(f.Tag)
	if e == nil || validFormat(format(e.slot), f.Value) {
		return nil
	}
	return &ValidationError{
		Err:    ErrIncorrectFormat,
		Tag:    f.Tag,
		Reason: ReasonIncorrectDataFormat,
		Msg:    fmt.Sprintf("value %q of tag %d%s is not a valid %s", f.Value, f.Tag, d.fieldName(f.Tag), d.Fields[f.Tag].Type),
	}
}

// fieldName returns " (Name)" for a tag the dictionary defines, "" otherwise.
func (d *Dict) fieldName(tag int) string {
	if fd := d.Fields[tag]; fd != nil {
		return " (" + fd.Name + ")"
	}
	return ""
}

// validFormat reports whether v, not empty, is in format f.
func validFormat(f format, v []byte) bool {
	switch f {
	case formatInt:
		if v[0] == '-' {
			v = v[1:]
		}
		return digits(v)
	case formatUint:
		return digits(v)
	case formatFloat:
		if v[0] == '-' {
			v = v[1:]
		}
		dot, n := false, 0
		for _, c := range v {
			switch {
			case c >= '0' && c <= '9':
				n++
			case c == '.' && !dot:
				dot = true
			default:
				return false
			}
		}
		return n > 0
	case formatChar:
		return len(v) == 1
	case formatBool:
		return len(v) == 1 && (v[0] == 'Y' || v[0] == 'N')
	case formatTimestamp:
		_, err := fix2.ParseUTCTimestamp(v)
		return err == nil
	case formatTimeOnly:
		_, err := fix2.ParseUTCTimeOnly(v)
		return err == nil
	case formatDate:
		_, err := fix2.ParseUTCDate(v)
		return err == nil && len(v) == 8
	case formatMonthYear:
		// YYYYMM, YYYYMMDD or YYYYMMwN (week N).
		if len(v) < 6 || !digits(v[:6]) || v[4] > '1' || v[4] == '1' && v[5] > '2' || v[4] == '0' && v[5] == '0' {
			return false
		}
		switch rest := v[6:]; {
		case len(rest) == 0:
			return true
		case len(rest) == 2 && rest[0] == 'w':
			return rest[1] >= '1' && rest[1] <= '5'
		case len(rest) == 2:
			_, err := fix2.ParseUTCDate(v)
			return err == nil
		}
		return false
	}
	return true
}

// digits reports whether v is one or more decimal digits.
func digits(v []byte) bool {
	if len(v) == 0 {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// checkStructure checks the top-level fields of msg, of message type mdef,
// for repeated tags, order and group counts, as checks select. groups tells
// whether msg holds a group counter.
func (d *Dict) checkStructure(msg *fix2.Message, mdef *MessageDef, groups bool, checks Checks) error {
	lookup := func(tag int) *fix2.GroupSpec {
		if e := d.lookup(mdef, tag); e != nil {
			return e.spec
		}
		return nil
	}
	fields := msg.TopLevelFieldsFunc(lookup)
	if !groups {
		fields = func(yield func(fix2.Field) bool) {
			for _, f := range msg.Fields() {
				if !yield(f) {
					return
				}
			}
		}
	}
	var seen map[int]bool
	if checks&CheckRepeatedTags != 0 {
		seen = make(map[int]bool, msg.Count())
	}
	phase := sectionHeader
	for f := range fields {
		e := d.lookup(mdef, f.Tag)
		if e == nil {
			continue // reported by Validate
		}
		if seen != nil {
			if seen[f.Tag] {
				return &ValidationError{
					Err:    ErrRepeatedTag,
					Tag:    f.Tag,
					Reason: ReasonTagAppearsMoreThanOnce,
					Msg:    fmt.Sprintf("tag %d%s appears more than once in message %s", f.Tag, d.fieldName(f.Tag), mdef.Name),
				}
			}
			seen[f.Tag] = true
		}
		if checks&CheckOrder != 0 {
			if e.section < phase {
				where := [...]string{"standard header", "body", "standard trailer"}
				return &ValidationError{
					Err:    ErrOutOfOrder,
					Tag:    f.Tag,
					Reason: ReasonTagSpecifiedOutOfOrder,
					Msg:    fmt.Sprintf("tag %d%s of the %s comes after the %s in message %s", f.Tag, d.fieldName(f.Tag), where[e.section], where[phase], mdef.Name),
				}
			}
			phase = e.section
		}
		if checks&CheckGroupCounts != 0 && e.spec != nil {
			if err := d.checkCount(f.Value, msg.GroupsOf(e.spec), e.spec, mdef); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCount checks that entries, the entries of the group of spec, match
// declared, its NumInGroup, and so do their nested groups.
func (d *Dict) checkCount(declared []byte, entries []fix2.Group, spec *fix2.GroupSpec, mdef *MessageDef) error {
	if n, err := fix2.ParseInt(declared); err != nil || n != len(entries) {
		return &ValidationError{
			Err:    ErrGroupCount,
			Tag:    spec.Counter,
			Reason: ReasonIncorrectNumInGroupCount,
			Msg:    fmt.Sprintf("NumInGroup %d%s is %q but %d entries follow in message %s", spec.Counter, d.fieldName(spec.Counter), declared, len(entries), mdef.Name),
		}
	}
	for _, entry := range entries {
		for _, nested := range spec.Nested {
			if v := entry.GetTopLevel(nested.Counter, spec.Nested...); v != nil {
				if err := d.checkCount(v, entry.GroupsOf(nested), nested, mdef); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

