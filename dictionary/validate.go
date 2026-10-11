package dictionary

import (
	"bytes"
	"fmt"
	"slices"

	fix2 "github.com/sylr/fix2"
)

// Compile-time check that Dict implements fix2.Validator.
var _ fix2.Validator = (*Dict)(nil)

// Sentinel errors for dictionary validation.
var (
	ErrValidation       = fmt.Errorf("%w: validation", fix2.ErrFix2)
	ErrUnknownMsgType   = fmt.Errorf("%w: unknown MsgType", ErrValidation)
	ErrRequiredTag      = fmt.Errorf("%w: required tag missing", ErrValidation)
	ErrUnexpectedTag    = fmt.Errorf("%w: unexpected tag", ErrValidation)
	ErrInvalidEnumValue = fmt.Errorf("%w: invalid enum value", ErrValidation)
	ErrEmptyValue       = fmt.Errorf("%w: tag without a value", ErrValidation)
	ErrIncorrectFormat  = fmt.Errorf("%w: incorrect data format", ErrValidation)
	ErrRepeatedTag      = fmt.Errorf("%w: tag appears more than once", ErrValidation)
	ErrOutOfOrder       = fmt.Errorf("%w: tag out of order", ErrValidation)
	ErrGroupCount       = fmt.Errorf("%w: incorrect NumInGroup count", ErrValidation)
)

// ValidationError wraps a sentinel error with the offending tag, the
// SessionRejectReason a session Reject would carry for it, and a message. Its
// Error text is the message prefixed with "fix2: ".
type ValidationError struct {
	Err    error
	Tag    int // the offending tag: 35 for an unknown MsgType
	Reason RejectReason
	Msg    string
}

func (e *ValidationError) Error() string {
	return fix2.ErrFix2.Error() + ": " + e.Msg
}

func (e *ValidationError) Unwrap() error {
	return e.Err
}

// Validate checks msg against the dictionary.
//
// It verifies that:
//  1. MsgType is known
//  2. All required header tags are present
//  3. All required trailer tags are present
//  4. All required message-body tags are present; required fields of an
//     optional component only when the component is present, i.e. when a
//     tag found nowhere else in the message is (components whose presence
//     cannot be told apart this way are not enforced)
//  5. Every tag is allowed (header + trailer + message fields + structural)
//  6. Enum values are valid for fields that define enums
//
// Required tags (2-4) must be present outside group entries: a tag a group
// entry holds does not stand for the message's own.
func (d *Dict) Validate(msg *fix2.Message) error {
	return d.validate(msg, 0)
}

// validate checks msg as Validate does, plus checks (see ValidateWith).
func (d *Dict) validate(msg *fix2.Message, checks Checks) error {
	// 1. MsgType must be known.
	mdef, ok := d.Messages[string(msg.MsgType())]
	if !ok {
		return &ValidationError{
			Err:    ErrUnknownMsgType,
			Tag:    35,
			Reason: ReasonInvalidMsgType,
			Msg:    fmt.Sprintf("unknown MsgType %q", msg.MsgType()),
		}
	}

	// Repeated tags and fields out of order first: QuickFIX engines find
	// them while parsing, before validating against the dictionary.
	if structure := checks & (CheckRepeatedTags | CheckOrder); structure != 0 {
		if err := d.checkStructure(msg, mdef, structure); err != nil {
			return err
		}
	}

	// Presence of the tracked tags (required ones and those activating
	// optional components), anywhere and at the top level.
	var small [2][4]uint64
	present, top := small[0][:], small[1][:]
	if n := (int(mdef.slots) + 63) / 64; n > len(small[0]) {
		present, top = make([]uint64, n), make([]uint64, n)
	}

	// One pass over every field records presence and finds the first tag
	// that is not allowed or has an invalid enum value (5 and 6), reported
	// after the required-tag checks.
	var fieldErr error
	groups := false
	for _, f := range msg.Fields() {
		e := d.lookup(mdef, f.Tag)
		if e == nil {
			if fieldErr == nil {
				fieldErr = d.unexpected(mdef, f.Tag)
			}
			continue
		}
		if e.slot >= 0 {
			present[e.slot/64] |= 1 << (e.slot % 64)
		}
		groups = groups || e.counter
		if fieldErr == nil && !e.structural && checks != 0 {
			fieldErr = d.checkValue(f, checks)
		}
		if fieldErr == nil && !e.structural {
			fieldErr = d.checkEnum(f)
		}
	}

	// Required tags must be present at the top level: a group entry's field
	// does not stand for a message's own. Without any group, every field is.
	if groups {
		for f := range msg.TopLevelFieldsFunc(func(tag int) *fix2.GroupSpec {
			if e := d.lookup(mdef, tag); e != nil {
				return e.spec
			}
			return nil
		}) {
			if e := d.lookup(mdef, f.Tag); e != nil && e.slot >= 0 {
				top[e.slot/64] |= 1 << (e.slot % 64)
			}
		}
	} else {
		copy(top, present)
	}

	// 2-4. Required header, trailer and message-body tags. Messages are
	// formatted only on error: building them would allocate on every call.
	for i, tags := range [...][]tracked{d.headerReq, d.trailerReq, mdef.requiredTags} {
		for _, t := range tags {
			if !has(top, t.slot) {
				var msg string
				switch i {
				case 0:
					msg = fmt.Sprintf("required header tag %d missing", t.tag)
				case 1:
					msg = fmt.Sprintf("required trailer tag %d missing", t.tag)
				default:
					msg = fmt.Sprintf("required tag %d missing for message %s", t.tag, mdef.Name)
				}
				return &ValidationError{Err: ErrRequiredTag, Tag: t.tag, Reason: ReasonRequiredTagMissing, Msg: msg}
			}
		}
	}

	// 4b. Required fields of optional components that are present: any of
	// their activation tags is, anywhere, since a component can show its
	// presence through one of its groups. A header or trailer activation tag
	// the message also declares in its body could be the body's.
	for i, conds := range [...][]compiledCond{d.headerConds, d.trailerConds, mdef.conds} {
		envelope := i < 2
		for _, c := range conds {
			if !slices.ContainsFunc(c.activation, func(t tracked) bool {
				if !has(present, t.slot) {
					return false
				}
				if envelope {
					if e := mdef.body.get(t.tag); e != nil && e.declared {
						return false
					}
				}
				return true
			}) {
				continue
			}
			for _, t := range c.required {
				if !has(top, t.slot) {
					where := [...]string{"header", "trailer", "message " + mdef.Name}[i]
					return &ValidationError{
						Err:    ErrRequiredTag,
						Tag:    t.tag,
						Reason: ReasonRequiredTagMissing,
						Msg:    fmt.Sprintf("required tag %d missing for component %s in %s", t.tag, c.component, where),
					}
				}
			}
		}
	}

	if fieldErr == nil && checks&CheckGroupCounts != 0 {
		return d.checkStructure(msg, mdef, CheckGroupCounts)
	}
	return fieldErr
}

// unexpected reports tag, which mdef does not allow: an invalid tag number if
// the dictionary does not define it at all, as QuickFIX and its acceptance
// tests have it (FIX's "undefined tag", reason 3, is less used), a tag not
// defined for the message type otherwise.
func (d *Dict) unexpected(mdef *MessageDef, tag int) *ValidationError {
	if fd := d.Fields[tag]; fd != nil {
		return &ValidationError{
			Err:    ErrUnexpectedTag,
			Tag:    tag,
			Reason: ReasonTagNotDefinedForMessageType,
			Msg:    fmt.Sprintf("tag %d (%s) not allowed in message %s", tag, fd.Name, mdef.Name),
		}
	}
	return &ValidationError{
		Err:    ErrUnexpectedTag,
		Tag:    tag,
		Reason: ReasonInvalidTagNumber,
		Msg:    fmt.Sprintf("tag %d not defined by the dictionary, in message %s", tag, mdef.Name),
	}
}

// lookup returns what the message, then the envelope, knows of tag, or nil
// if the message does not allow it.
func (d *Dict) lookup(mdef *MessageDef, tag int) *tagEntry {
	if e := mdef.body.get(tag); e != nil {
		return e
	}
	return d.envelope.get(tag)
}

// has reports whether slot is set in bits.
func has(bits []uint64, slot int32) bool {
	return bits[slot/64]&(1<<(slot%64)) != 0
}

// checkEnum returns an error if f's field has enums and f's value, or one of
// its space-separated values for a multiple-value field, is not one of them.
func (d *Dict) checkEnum(f fix2.Field) error {
	e := d.enums.get(f.Tag)
	if e == nil {
		return nil
	}
	ec := &d.enumChecks[e.slot]
	if !ec.multi {
		if ec.valid(f.Value) {
			return nil
		}
		return &ValidationError{
			Err:    ErrInvalidEnumValue,
			Tag:    f.Tag,
			Reason: ReasonValueIsIncorrect,
			Msg:    fmt.Sprintf("invalid value %q for tag %d (%s)", f.Value, f.Tag, ec.name),
		}
	}
	for item := range bytes.SplitSeq(f.Value, space) {
		if !ec.valid(item) {
			return &ValidationError{
				Err:    ErrInvalidEnumValue,
				Tag:    f.Tag,
				Reason: ReasonValueIsIncorrect,
				Msg:    fmt.Sprintf("invalid value %q in %q for tag %d (%s)", item, f.Value, f.Tag, ec.name),
			}
		}
	}
	return nil
}

var space = []byte{' '}

// isMultipleValue reports whether fixType holds space-separated values:
// MULTIPLEVALUESTRING in FIX 4.2-4.4, MULTIPLECHARVALUE and
// MULTIPLESTRINGVALUE from FIX 4.4/5.0 on.
func isMultipleValue(fixType string) bool {
	switch fixType {
	case "MULTIPLEVALUESTRING", "MULTIPLECHARVALUE", "MULTIPLESTRINGVALUE":
		return true
	}
	return false
}

// groupSpecs returns the specs of groups. An entry may also hold the LENGTH
// field of its DATA fields, which the parser requires right before them, and
// the DATA field of its LENGTH fields: generated builders write both, even
// when the dictionary declares only one.
func (d *Dict) groupSpecs(groups []GroupDef) []*fix2.GroupSpec {
	specs := make([]*fix2.GroupSpec, 0, len(groups))
	for _, gd := range groups {
		s := &fix2.GroupSpec{Counter: gd.CounterTag, Delim: gd.DelimTag}
		// LENGTH comes right before its DATA field, declared or not, so an
		// entry whose first field is DATA starts with LENGTH.
		if l := d.profile.DataLengthTag(gd.DelimTag); l != 0 {
			s.Delim = l
		}
		added := make(map[int]bool, len(gd.Fields))
		add := func(tag int) {
			if !added[tag] {
				added[tag] = true
				s.Tags = append(s.Tags, tag)
			}
		}
		for _, fr := range gd.Fields {
			if l := d.profile.DataLengthTag(fr.Tag); l != 0 {
				add(l)
			}
			add(fr.Tag)
			if dt := d.profile.DataTag(fr.Tag); dt != 0 {
				add(dt)
			}
		}
		s.Nested = d.groupSpecs(gd.Groups)
		specs = append(specs, s)
	}
	return specs
}
