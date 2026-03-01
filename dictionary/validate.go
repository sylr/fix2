package dictionary

import (
	"fmt"

	fix2 "github.com/sylr/fix2"
)

// Compile-time check that Dict implements fix2.Validator.
var _ fix2.Validator = (*Dict)(nil)

// Sentinel errors for dictionary validation.
var (
	ErrValidation       = fmt.Errorf("fix2: validation")
	ErrUnknownMsgType   = fmt.Errorf("%w: unknown MsgType", ErrValidation)
	ErrRequiredTag      = fmt.Errorf("%w: required tag missing", ErrValidation)
	ErrUnexpectedTag    = fmt.Errorf("%w: unexpected tag", ErrValidation)
	ErrInvalidEnumValue = fmt.Errorf("%w: invalid enum value", ErrValidation)
)

// ValidationError wraps a sentinel error with the offending tag and a message.
type ValidationError struct {
	Err error
	Tag int
	Msg string
}

func (e *ValidationError) Error() string {
	return e.Msg
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
//  4. All required message-body tags are present
//  5. Every tag is allowed (header + trailer + message fields + structural)
//  6. Enum values are valid for fields that define enums
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

	// Collect present tags for required-field checks.
	present := make(map[int]bool, msg.Count())
	for _, f := range msg.Fields() {
		present[f.Tag] = true
	}

	// 2. Required header tags.
	for tag := range d.headerReq {
		if !present[tag] {
			return &ValidationError{
				Err: ErrRequiredTag,
				Tag: tag,
				Msg: fmt.Sprintf("required header tag %d missing", tag),
			}
		}
	}

	// 3. Required trailer tags.
	for tag := range d.trailerReq {
		if !present[tag] {
			return &ValidationError{
				Err: ErrRequiredTag,
				Tag: tag,
				Msg: fmt.Sprintf("required trailer tag %d missing", tag),
			}
		}
	}

	// 4. Required message-body tags.
	for tag := range mdef.required {
		if !present[tag] {
			return &ValidationError{
				Err: ErrRequiredTag,
				Tag: tag,
				Msg: fmt.Sprintf("required tag %d missing for message %s", tag, mdef.Name),
			}
		}
	}

	// 5 & 6. Check each tag is allowed and enum values are valid.
	for _, f := range msg.Fields() {
		tag := f.Tag

		// Structural tags are always allowed.
		if structuralTags[tag] {
			continue
		}

		// Tag must be in header, trailer, or message fields.
		if !d.headerSet[tag] && !d.trailerSet[tag] && !mdef.fieldSet[tag] {
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
			if _, ok := fd.Enums[val]; !ok {
				return &ValidationError{
					Err: ErrInvalidEnumValue,
					Tag: tag,
					Msg: fmt.Sprintf("invalid value %q for tag %d (%s)", val, tag, fd.Name),
				}
			}
		}
	}

	return nil
}
