package fix2

import (
	"errors"
	"fmt"
)

var (
	ErrFix2                   = fmt.Errorf("fix2")
	ErrMessageTooShort        = fmt.Errorf("%w: message too short", ErrFix2)
	ErrInvalidBeginTag        = fmt.Errorf("%w: invalid BeginString (tag 8)", ErrFix2)
	ErrInvalidBodyLen         = fmt.Errorf("%w: invalid BodyLength (tag 9)", ErrFix2)
	ErrMissingMsgType         = fmt.Errorf("%w: MsgType (tag 35) is not the third field", ErrFix2)
	ErrInvalidCheckSum        = fmt.Errorf("%w: invalid CheckSum (tag 10)", ErrFix2)
	ErrChecksumMismatch       = fmt.Errorf("%w: checksum mismatch", ErrFix2)
	ErrBodyLenMismatch        = fmt.Errorf("%w: body length mismatch", ErrFix2)
	ErrMalformedField         = fmt.Errorf("%w: malformed field", ErrFix2)
	ErrInvalidTag             = fmt.Errorf("%w: invalid tag number", ErrFix2)
	ErrFieldSeparatorNotFound = fmt.Errorf("%w: field separator not found where expected", ErrFix2)
)

// ParseError wraps a sentinel error with additional context.
type ParseError struct {
	Err    error
	Offset int
}

func (e *ParseError) Error() string {
	return e.Err.Error()
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

func (e *ParseError) Is(target error) bool {
	return errors.Is(e.Err, target)
}
