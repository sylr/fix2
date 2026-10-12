package dictionary

import (
	"fmt"
	"slices"

	fix2 "github.com/sylr/fix2"
)

// Checks selects the validations ValidateWith runs besides those Validate
// always does. Validate runs CheckUnknownTags and CheckUserDefinedTags too.
// A session enables them all by default, as QuickFIX does.
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
	// CheckUnknownTags rejects tags below 5000 the message does not define,
	// its header and trailer included: a tag the dictionary does not define
	// (reason 0), or not for the message's type (reason 2). Without it, they
	// are accepted, as QuickFIX/J's AllowUnknownMsgFields=Y does.
	CheckUnknownTags
	// CheckUserDefinedTags rejects user-defined tags, 5000 and up, the
	// message does not define, as CheckUnknownTags those below. Without it,
	// they are accepted, as QuickFIX/J's ValidateUserDefinedFields=N does.
	CheckUserDefinedTags

	// CheckAll enables every check.
	CheckAll = CheckEmptyValues | CheckFormats | CheckRepeatedTags | CheckOrder | CheckGroupCounts | CheckUnknownTags | CheckUserDefinedTags

	// checksValidate are the checks Validate runs.
	checksValidate = CheckUnknownTags | CheckUserDefinedTags
	// checksValue are the checks of a field's value.
	checksValue = CheckEmptyValues | CheckFormats
)

// userDefinedTagMin is the first user-defined tag.
const userDefinedTagMin = 5000

// ValidateWith checks msg as Validate does, with the checks selected rather
// than CheckUnknownTags and CheckUserDefinedTags. A repeated tag or a field
// out of order is reported first, as QuickFIX engines find them while
// parsing; then missing required tags; then field errors in message order;
// then group counts. Unlike Validate, the extra checks may allocate. To
// reject unknown tags as Validate does, select their checks too:
//
//	d.ValidateWith(msg, dictionary.CheckFormats|dictionary.CheckUnknownTags|dictionary.CheckUserDefinedTags)
//
// A group entry keeps a tag the message does not define, whether checked
// or not: FIX does not say where such a tag belongs, and QuickFIX/J takes
// it for the entry's. One right after a group's last entry is taken for its
// too.
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
	formatTimestampSec
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
	case "UTCTIMESTAMP":
		return formatTimestamp
	case "TIME":
		// FIX 4.0 and 4.1 timestamps have whole seconds.
		return formatTimestampSec
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
	case formatTimestampSec:
		_, err := fix2.ParseUTCTimestamp(v)
		return err == nil && len(v) == len(fix2.TimeFormatTimestampSec)
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
// for repeated tags, order and group counts, as checks select. Group entries
// are skipped by walking each group from its counter, which counts them.
func (d *Dict) checkStructure(msg *fix2.Message, mdef *MessageDef, checks Checks) error {
	fields := msg.Fields()
	var seen map[int]bool
	if checks&CheckRepeatedTags != 0 {
		seen = make(map[int]bool, len(fields))
	}
	phase := sectionHeader
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		e := d.lookup(mdef, f.Tag)
		if e == nil {
			// Reported by validate if checked; a repeated one otherwise.
			if seen != nil && checks&unknownCheck(f.Tag) == 0 {
				if seen[f.Tag] {
					return d.repeated(f.Tag, mdef)
				}
				seen[f.Tag] = true
			}
			continue
		}
		if seen != nil {
			if seen[f.Tag] {
				return d.repeated(f.Tag, mdef)
			}
			seen[f.Tag] = true
		}
		if checks&CheckOrder != 0 && e.section != sectionAny {
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
		if e.spec != nil {
			next, err := d.walkGroup(fields, i, e.spec, mdef, checks&CheckGroupCounts != 0)
			if err != nil {
				return err
			}
			i = next - 1
		}
	}
	return nil
}

// repeated reports tag, which appears more than once in a message of type
// mdef outside group entries.
func (d *Dict) repeated(tag int, mdef *MessageDef) *ValidationError {
	return &ValidationError{
		Err:    ErrRepeatedTag,
		Tag:    tag,
		Reason: ReasonTagAppearsMoreThanOnce,
		Msg:    fmt.Sprintf("tag %d%s appears more than once in message %s", tag, d.fieldName(tag), mdef.Name),
	}
}

// walkGroup walks the entries of the group whose counter is fields[at], of
// spec, and returns the index of the first field after them. If check is
// set, it returns an error when the counter does not match the number of
// entries, or a nested group's counter its own.
func (d *Dict) walkGroup(fields []fix2.Field, at int, spec *fix2.GroupSpec, mdef *MessageDef, check bool) (int, error) {
	entries, i := 0, at+1
	for i < len(fields) {
		tag := fields[i].Tag
		if tag == spec.Delim {
			entries++
		} else if entries == 0 || !slices.Contains(spec.Tags, tag) && spec.Child(tag) == nil && (spec.Unknown == nil || !spec.Unknown(tag)) {
			break
		}
		// The delimiter itself may be a nested group's counter.
		if child := spec.Child(tag); child != nil {
			next, err := d.walkGroup(fields, i, child, mdef, check)
			if err != nil {
				return 0, err
			}
			i = next
			continue
		}
		i++
	}
	if check {
		if n, err := fix2.ParseInt(fields[at].Value); err != nil || n != entries {
			return 0, &ValidationError{
				Err:    ErrGroupCount,
				Tag:    spec.Counter,
				Reason: ReasonIncorrectNumInGroupCount,
				Msg:    fmt.Sprintf("NumInGroup %d%s is %q but %d entries follow in message %s", spec.Counter, d.fieldName(spec.Counter), fields[at].Value, entries, mdef.Name),
			}
		}
	}
	return i, nil
}
