package dictionary

import (
	"fmt"
	"slices"
	"strconv"

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
	// match their entries (reason 16): fewer follow it than it says, or
	// another follows them, its delimiter a tag the enclosing entry or the
	// message could not hold there.
	CheckGroupCounts
	// CheckUnknownTags rejects tags below 5000 the message does not define,
	// its header and trailer included: a tag the dictionary does not define
	// (reason 0), or not for the message's type (reason 2). Without it, they
	// are accepted, as QuickFIX/J's AllowUnknownMsgFields=Y does: so is a
	// tag the message holds only in group entries, outside them. The stray
	// fields of a group with more entries than its NumInGroup says are then
	// accepted too: CheckGroupCounts alone reports the count.
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
// then group counts; then a tag held only in group entries outside them, if
// its tag's check, CheckUnknownTags or CheckUserDefinedTags, is selected.
// Unlike Validate, the extra checks may allocate. To reject unknown tags as
// Validate does, select their checks too:
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
	var walk *groupWalk
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
			if walk == nil {
				walk = &groupWalk{d: d, mdef: mdef, fields: fields, check: checks&CheckGroupCounts != 0, seen: map[entryTag]int{}}
			}
			next, err := walk.group(i, e.spec, nil)
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

// groupWalk walks the groups of a message's fields as GroupsOf reads them:
// a group holds at most as many entries as its counter says, and an entry
// ends at the next one, at a tag its group does not hold, or at one it
// already holds.
type groupWalk struct {
	d      *Dict
	mdef   *MessageDef
	fields []fix2.Field
	check  bool // report counters not matching their entries
	// seen maps a tag of a group's entries to the index of the delimiter of
	// the last entry holding it.
	seen map[entryTag]int
}

// entryTag is a tag of the entries of a group.
type entryTag struct {
	spec *fix2.GroupSpec
	tag  int
}

// group walks the entries of the group whose counter is fields[at], of spec,
// in an entry of parent or, if parent is nil, outside groups, and returns
// the index of the first field after them. If w.check is set, it returns an
// error when the counter does not match the number of entries, or a nested
// group's counter its own: when there are fewer, or when another entry
// follows them, whose delimiter parent's entry or the message could not hold
// there instead.
func (w *groupWalk) group(at int, spec, parent *fix2.GroupSpec) (int, error) {
	count, cerr := fix2.ParseInt(w.fields[at].Value)
	i, n := at+1, 0
	for ; n < count && i < len(w.fields) && w.fields[i].Tag == spec.Delim; n++ {
		var err error
		if i, err = w.entry(i, spec); err != nil {
			return 0, err
		}
	}
	if !w.check {
		return i, nil
	}
	more := n == count && i < len(w.fields) && w.fields[i].Tag == spec.Delim && !w.holds(parent, spec.Delim)
	if cerr == nil && n == count && !more {
		return i, nil
	}
	follow := strconv.Itoa(n)
	if more {
		follow = "more"
	}
	return 0, &ValidationError{
		Err:    ErrGroupCount,
		Tag:    spec.Counter,
		Reason: ReasonIncorrectNumInGroupCount,
		Msg:    fmt.Sprintf("NumInGroup %d%s is %q but %s entries follow in message %s", spec.Counter, w.d.fieldName(spec.Counter), w.fields[at].Value, follow, w.mdef.Name),
	}
}

// entry walks the entry of spec whose delimiter is at fields[i] and returns
// the index just past it.
func (w *groupWalk) entry(i int, spec *fix2.GroupSpec) (int, error) {
	start := i
	for i < len(w.fields) {
		tag := w.fields[i].Tag
		child := spec.Child(tag)
		held := child != nil || slices.Contains(spec.Tags, tag)
		if i > start && (tag == spec.Delim || !held && (spec.Unknown == nil || !spec.Unknown(tag))) {
			break // the next entry, or not part of this group
		}
		if held {
			k := entryTag{spec, tag}
			if s, ok := w.seen[k]; ok && s == start {
				break // a tag cannot repeat within an entry
			}
			w.seen[k] = start
		}
		// The delimiter itself may be a nested group's counter.
		if child != nil {
			next, err := w.group(i, child, spec)
			if err != nil {
				return 0, err
			}
			i = next
			continue
		}
		i++
	}
	return i, nil
}

// holds reports whether an entry of parent or, if parent is nil, the message
// outside groups may hold tag.
func (w *groupWalk) holds(parent *fix2.GroupSpec, tag int) bool {
	if parent == nil {
		e := w.d.lookup(w.mdef, tag)
		return e != nil && !e.grouped
	}
	return slices.Contains(parent.Tags, tag) || parent.Child(tag) != nil || parent.Unknown != nil && parent.Unknown(tag)
}
