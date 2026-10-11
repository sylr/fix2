package fix2

import "errors"

var (
	ErrFix2                   = errors.New("fix2")
	ErrMessageTooShort        = newError("message too short")
	ErrInvalidBeginTag        = newError("invalid BeginString (tag 8)")
	ErrInvalidBodyLen         = newError("invalid BodyLength (tag 9)")
	ErrMissingMsgType         = newError("MsgType (tag 35) is not the third field")
	ErrInvalidCheckSum        = newError("invalid CheckSum (tag 10)")
	ErrChecksumMismatch       = newError("checksum mismatch")
	ErrBodyLenMismatch        = newError("body length mismatch")
	ErrMessageTooLarge        = newError("message too large")
	ErrMalformedField         = newError("malformed field")
	ErrInvalidTag             = newError("invalid tag number")
	ErrInvalidField           = newError("invalid field")
	ErrFieldSeparatorNotFound = newError("field separator not found where expected")
	// ErrUnsupportedMessageType reports a message type an application does
	// not handle: a session answers it with a BusinessMessageReject
	// ("Unsupported Message Type"). Generated Dispatch functions return it.
	ErrUnsupportedMessageType = newError("unsupported message type")
)

// fix2Error is a sentinel that unwraps to ErrFix2. Its message has no "fix2: "
// prefix so sentinels can be chained without repeating it; ParseError adds the
// prefix once.
type fix2Error struct{ msg string }

func newError(msg string) error { return &fix2Error{msg: msg} }

func (e *fix2Error) Error() string { return e.msg }

func (e *fix2Error) Unwrap() error { return ErrFix2 }

// ParseError wraps a sentinel error with additional context.
type ParseError struct {
	Err    error
	Offset int
}

func (e *ParseError) Error() string {
	return ErrFix2.Error() + ": " + e.Err.Error()
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

func (e *ParseError) Is(target error) bool {
	return errors.Is(e.Err, target)
}
