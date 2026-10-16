package fix

// Field represents a single FIX tag=value pair.
// Value is a zero-copy slice into the original message buffer.
type Field struct {
	Tag   int
	Value []byte
}
