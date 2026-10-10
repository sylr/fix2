package dictionary

import (
	"fmt"
	"slices"
	"strings"

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
)

// ValidationError wraps a sentinel error with the offending tag and a message.
// Its Error text is the message prefixed with "fix2: ".
type ValidationError struct {
	Err error
	Tag int
	Msg string
}

func (e *ValidationError) Error() string {
	return fix2.ErrFix2.Error() + ": " + e.Msg
}

func (e *ValidationError) Unwrap() error {
	return e.Err
}

// Structural tags that the parser manages; they are always allowed regardless
// of dictionary content.
var structuralTags = map[int]bool{
	8:  true, // BeginString
	9:  true, // BodyLength
	10: true, // CheckSum
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
	msgType := string(msg.MsgType())

	// 1. MsgType must be known.
	mdef, ok := d.Messages[msgType]
	if !ok {
		return &ValidationError{
			Err: ErrUnknownMsgType,
			Msg: fmt.Sprintf("unknown MsgType %q", msgType),
		}
	}

	// Required tags must be present at the top level: a group entry's field
	// does not stand for a message's own. Any occurrence, though, shows an
	// optional component is present, its unique tags possibly being in one
	// of its groups.
	present := make(map[int]bool, msg.Count())
	for _, f := range msg.Fields() {
		present[f.Tag] = true
	}
	top := make(map[int]bool, msg.Count())
	specs := mdef.specs
	switch {
	case len(specs) == 0:
		specs = d.envelopeSpecs
	case len(d.envelopeSpecs) > 0:
		specs = slices.Concat(d.envelopeSpecs, specs)
	}
	for f := range msg.TopLevelFields(specs...) {
		top[f.Tag] = true
	}

	// 2. Required header tags.
	for tag := range d.headerReq {
		if !top[tag] {
			return &ValidationError{
				Err: ErrRequiredTag,
				Tag: tag,
				Msg: fmt.Sprintf("required header tag %d missing", tag),
			}
		}
	}

	// 3. Required trailer tags.
	for tag := range d.trailerReq {
		if !top[tag] {
			return &ValidationError{
				Err: ErrRequiredTag,
				Tag: tag,
				Msg: fmt.Sprintf("required trailer tag %d missing", tag),
			}
		}
	}

	// 4. Required message-body tags.
	for tag := range mdef.required {
		if !top[tag] {
			return &ValidationError{
				Err: ErrRequiredTag,
				Tag: tag,
				Msg: fmt.Sprintf("required tag %d missing for message %s", tag, mdef.Name),
			}
		}
	}

	// 4b. Required fields of optional components that are present.
	for _, cc := range []struct {
		conds []condition
		where string
	}{
		{d.headerConds, "header"},
		{d.trailerConds, "trailer"},
		{mdef.conditions, "message " + mdef.Name},
	} {
		if err := checkConditions(cc.conds, present, top, cc.where); err != nil {
			return err
		}
	}

	// 5 & 6. Check each tag is allowed and enum values are valid.
	allowed := func(tag int) bool { return d.headerSet[tag] || d.trailerSet[tag] || mdef.fieldSet[tag] }
	paired := func(other int) bool { return other != 0 && allowed(other) }
	for _, f := range msg.Fields() {
		tag := f.Tag

		// Structural tags are always allowed.
		if structuralTags[tag] {
			continue
		}

		// Tag must be in header, trailer, or message fields, or pair with
		// such a field: the parser requires LENGTH right before its DATA
		// field, and generated builders write both, even when the
		// dictionary declares only one.
		if !allowed(tag) && !paired(fix2.DataTag(tag)) && !paired(fix2.DataLengthTag(tag)) {
			return &ValidationError{
				Err: ErrUnexpectedTag,
				Tag: tag,
				Msg: fmt.Sprintf("tag %d not allowed in message %s", tag, mdef.Name),
			}
		}

		// Enum validation.
		fd := d.Fields[tag]
		if fd != nil && fd.Enums != nil {
			val := string(f.Value)
			if !isMultipleValue(fd.Type) {
				if _, ok := fd.Enums[val]; !ok {
					return &ValidationError{
						Err: ErrInvalidEnumValue,
						Tag: tag,
						Msg: fmt.Sprintf("invalid value %q for tag %d (%s)", val, tag, fd.Name),
					}
				}
				continue
			}
			// Multiple-value fields hold space-separated enum values.
			for item := range strings.SplitSeq(val, " ") {
				if _, ok := fd.Enums[item]; !ok {
					return &ValidationError{
						Err: ErrInvalidEnumValue,
						Tag: tag,
						Msg: fmt.Sprintf("invalid value %q in %q for tag %d (%s)", item, val, tag, fd.Name),
					}
				}
			}
		}
	}

	return nil
}

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

// checkConditions returns an error if an optional component is present (any
// of its activation tags is, anywhere) but one of its required tags is
// missing from the top level.
func checkConditions(conds []condition, present, top map[int]bool, where string) error {
	for _, c := range conds {
		active := false
		for _, tag := range c.activation {
			if present[tag] {
				active = true
				break
			}
		}
		if !active {
			continue
		}
		for _, tag := range c.required {
			if !top[tag] {
				return &ValidationError{
					Err: ErrRequiredTag,
					Tag: tag,
					Msg: fmt.Sprintf("required tag %d missing for component %s in %s", tag, c.component, where),
				}
			}
		}
	}
	return nil
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
		if l := fix2.DataLengthTag(gd.DelimTag); l != 0 {
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
			if l := fix2.DataLengthTag(fr.Tag); l != 0 {
				add(l)
			}
			add(fr.Tag)
			if dt := fix2.DataTag(fr.Tag); dt != 0 {
				add(dt)
			}
		}
		s.Nested = d.groupSpecs(gd.Groups)
		specs = append(specs, s)
	}
	return specs
}
