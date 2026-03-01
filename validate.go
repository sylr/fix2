package fix2

import "io"

// Validator validates a parsed FIX message against a set of rules
// (e.g. a data dictionary).
type Validator interface {
	Validate(msg *Message) error
}

// ParseAndValidate parses data as a FIX message, then validates it using v.
func ParseAndValidate(data []byte, v Validator) (Message, error) {
	msg, err := Parse(data)
	if err != nil {
		return msg, err
	}
	if err := v.Validate(&msg); err != nil {
		return msg, err
	}
	return msg, nil
}

// ParseFromReaderAndValidate reads a single FIX message from r, parses it,
// then validates it using v.
func ParseFromReaderAndValidate(r io.Reader, v Validator) (Message, error) {
	msg, err := ParseFromReader(r)
	if err != nil {
		return msg, err
	}
	if err := v.Validate(&msg); err != nil {
		return msg, err
	}
	return msg, nil
}
