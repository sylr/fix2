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
	// CheckGroupRequired rejects group entries without their group's
	// required fields or nested groups (reason 1), as QuickFIX does, or
	// without the required fields of an optional component they hold, as
	// Validate checks a message's, which QuickFIX does not.
	CheckGroupRequired
	// CheckGroupOrder rejects group entries whose fields are not in their
	// group's order (reason 15), as QuickFIX/J's
	// ValidateUnorderedGroupFields=Y does. A DATA field and its LENGTH
	// field count as one; tags the group does not define are not ordered.
	CheckGroupOrder

	// CheckAll enables every check.
	CheckAll = CheckEmptyValues | CheckFormats | CheckRepeatedTags | CheckOrder | CheckGroupCounts | CheckUnknownTags | CheckUserDefinedTags | CheckGroupRequired | CheckGroupOrder

	// checksValidate are the checks Validate runs.
	checksValidate = CheckUnknownTags | CheckUserDefinedTags
	// checksValue are the checks of a field's value.
	checksValue = CheckEmptyValues | CheckFormats
)

// userDefinedTagMin is the first user-defined tag.
const userDefinedTagMin = 5000

// ValidateWith checks msg as Validate does, with the checks selected rather
// than CheckUnknownTags and CheckUserDefinedTags. A repeated tag or a field
// out of order, a group entry's included, is reported first, as QuickFIX
// engines find them while parsing; then missing required tags; then field
// errors in message order; then group counts and group entries missing
// required fields, in message order; then a tag held only in group entries
// outside them, if
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
	walk := groupWalk{d: d, mdef: mdef, fields: fields, check: checks&CheckGroupCounts != 0,
		required: checks&CheckGroupRequired != 0, order: checks&CheckGroupOrder != 0}
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
	d        *Dict
	mdef     *MessageDef
	fields   []fix2.Field
	check    bool // report counters not matching their entries
	required bool // report entries without their required fields
	order    bool // report entries with fields out of order
}

// entryState is the entry of a group being walked: the tags it holds so
// far, and the entry holding its group, nil outside groups.
type entryState struct {
	spec   *fix2.GroupSpec
	seen   []uint64 // bits by position in spec.Tags, then spec.Nested
	parent *entryState
}

// position returns the position of tag in spec.Tags, then spec.Nested, a
// nested counter first as GroupsOf has it, or -1, and the spec of the nested
// group it counts, if any.
func position(spec *fix2.GroupSpec, tag int) (int, *fix2.GroupSpec) {
	for c, n := range spec.Nested {
		if n.Counter == tag {
			return len(spec.Tags) + c, n
		}
	}
	return slices.Index(spec.Tags, tag), nil
}

// group walks the entries of the group whose counter is fields[at], of spec,
// in the entry parent or, if it is nil, outside groups, and returns the
// index of the first field after them. If w.check is set, it returns an
// error when the counter does not match the number of entries, or a nested
// group's counter its own: when there are fewer, or when another entry
// follows them, whose delimiter no enclosing entry nor the message could
// hold there instead.
func (w *groupWalk) group(at int, spec *fix2.GroupSpec, parent *entryState) (int, error) {
	count, cerr := fix2.ParseInt(w.fields[at].Value)
	// The tags of an entry, on the stack for groups of up to 256 tags and
	// nested groups.
	var small [4]uint64
	// The rules stay out of e: an error built from them must not make
	// every entryState, linked to its parent, escape.
	rules := w.d.groupRules[spec]
	e := entryState{spec: spec, seen: small[:], parent: parent}
	if n := (len(spec.Tags) + len(spec.Nested) + 63) / 64; n > len(small) {
		e.seen = make([]uint64, n)
	}
	i, n := at+1, 0
	// The first entry missing a required field, reported after the
	// counter, which comes before it.
	var missing error
	for ; n < count && i < len(w.fields) && w.fields[i].Tag == spec.Delim; n++ {
		var err error
		if i, err = w.entry(i, &e, rules, n+1); err != nil {
			return 0, err
		}
		if w.required && rules != nil && missing == nil {
			missing = w.complete(&e, rules, n+1)
		}
	}
	more := w.check && n == count && i < len(w.fields) && w.fields[i].Tag == spec.Delim && !w.holds(parent, spec.Delim)
	if !w.check || cerr == nil && n == count && !more {
		if missing != nil {
			return 0, missing
		}
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

// entry walks the entry of e.spec, of rules, whose delimiter is at
// fields[i], the group's nth, and returns the index just past it.
func (w *groupWalk) entry(i int, e *entryState, rules *groupRules, nth int) (int, error) {
	clear(e.seen)
	spec := e.spec
	last := int32(-1) // the rank of the last field ordered
	for start := i; i < len(w.fields); {
		tag := w.fields[i].Tag
		k, child := position(spec, tag)
		if i > start && (tag == spec.Delim || k < 0 && (spec.Unknown == nil || !spec.Unknown(tag))) {
			break // the next entry, or not part of this group
		}
		if k >= 0 {
			if e.seen[k/64]&(1<<(k%64)) != 0 {
				break // a tag cannot repeat within an entry
			}
			e.seen[k/64] |= 1 << (k % 64)
			if w.order && rules != nil {
				if r := rules.rank[k]; r >= 0 {
					if r < last {
						return 0, w.entryError(spec.Counter, rules, nth, ErrGroupOrder, ReasonRepeatingGroupOutOfOrder, tag,
							fmt.Sprintf("tag %d%s out of order", tag, w.d.fieldName(tag)))
					}
					last = r
				}
			}
		}
		// The delimiter itself may be a nested group's counter.
		if child != nil {
			next, err := w.group(i, child, e)
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

// complete returns an error if e, the nth entry of its group, of rules,
// misses a required field or nested group, or one of an optional component
// it holds.
func (w *groupWalk) complete(e *entryState, rules *groupRules, nth int) error {
	for _, k := range rules.required {
		if !bitSet(e.seen, k) {
			tag := tagAt(e.spec, k)
			return w.entryError(e.spec.Counter, rules, nth, ErrRequiredTag, ReasonRequiredTagMissing, tag,
				fmt.Sprintf("required tag %d%s missing", tag, w.d.fieldName(tag)))
		}
	}
	for _, c := range rules.conds {
		if !anySet(e.seen, c.activation) {
			continue
		}
		for _, k := range c.required {
			if !bitSet(e.seen, k) {
				tag := tagAt(e.spec, k)
				return w.entryError(e.spec.Counter, rules, nth, ErrRequiredTag, ReasonRequiredTagMissing, tag,
					fmt.Sprintf("required tag %d%s missing for component %s", tag, w.d.fieldName(tag), c.component))
			}
		}
	}
	return nil
}

// bitSet reports whether bit k of bits is set.
func bitSet(bits []uint64, k int) bool { return bits[k/64]&(1<<(k%64)) != 0 }

// anySet reports whether any of the bits ks of bits is set.
func anySet(bits []uint64, ks []int) bool {
	for _, k := range ks {
		if bitSet(bits, k) {
			return true
		}
	}
	return false
}

// entryError returns the error err, for reason, about tag in the nth entry
// of the group of counter, of rules, what says what.
func (w *groupWalk) entryError(counter int, rules *groupRules, nth int, err error, reason RejectReason, tag int, what string) *ValidationError {
	return &ValidationError{
		Err:    err,
		Tag:    tag,
		Reason: reason,
		Msg: fmt.Sprintf("%s in entry %d of group %d (%s) in message %s",
			what, nth, counter, rules.name, w.mdef.Name),
	}
}

// holds reports whether tag, ending a group's entries in the entry e, may
// belong to e, an entry enclosing it or, past them all, the message outside
// groups, as GroupsOf reads it: an entry keeps a tag its group holds and it
// does not hold yet, or one Unknown reports; its group's delimiter starts
// its next entry. Any other tag ends it.
func (w *groupWalk) holds(e *entryState, tag int) bool {
	for ; e != nil; e = e.parent {
		if tag == e.spec.Delim {
			return true
		}
		if k, _ := position(e.spec, tag); k >= 0 {
			if e.seen[k/64]&(1<<(k%64)) == 0 {
				return true
			}
		} else if e.spec.Unknown != nil && e.spec.Unknown(tag) {
			return true
		}
	}
	te := w.d.lookup(w.mdef, tag)
	return te != nil && !te.grouped
}
