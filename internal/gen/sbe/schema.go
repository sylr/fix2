package sbe

import (
	"fmt"
	"math"
	"strconv"
)

// Primitive is an SBE primitive type (§4.4.1, §4.6.1, §4.7.1).
type Primitive int

// The primitive types of SBE 1.0.
const (
	Char Primitive = iota + 1
	Int8
	Int16
	Int32
	Int64
	Uint8
	Uint16
	Uint32
	Uint64
	Float
	Double
)

var primitiveNames = map[string]Primitive{
	"char": Char, "int8": Int8, "int16": Int16, "int32": Int32, "int64": Int64,
	"uint8": Uint8, "uint16": Uint16, "uint32": Uint32, "uint64": Uint64,
	"float": Float, "double": Double,
}

func (p Primitive) String() string {
	for name, q := range primitiveNames {
		if q == p {
			return name
		}
	}
	return "primitive(" + strconv.Itoa(int(p)) + ")"
}

// Size is the octets a value of p takes on the wire.
func (p Primitive) Size() int {
	switch p {
	case Char, Int8, Uint8:
		return 1
	case Int16, Uint16:
		return 2
	case Int32, Uint32, Float:
		return 4
	}
	return 8
}

// Signed reports whether p is a signed integer.
func (p Primitive) Signed() bool { return p >= Int8 && p <= Int64 }

// Unsigned reports whether p is an unsigned integer.
func (p Primitive) Unsigned() bool { return p >= Uint8 && p <= Uint64 }

// Integer reports whether p is an integer, char included.
func (p Primitive) Integer() bool { return p != Float && p != Double }

// defaultNull is the null value of p when a schema gives none (§4.4.2,
// §4.6.2, §4.7.1.1): the lowest value of a signed integer, the highest of
// an unsigned one, NUL for a char, NaN for a float.
func (p Primitive) defaultNull() string {
	switch p {
	case Char:
		return "0"
	case Int8:
		return strconv.Itoa(math.MinInt8)
	case Int16:
		return strconv.Itoa(math.MinInt16)
	case Int32:
		return strconv.Itoa(math.MinInt32)
	case Int64:
		return strconv.FormatInt(math.MinInt64, 10)
	case Uint8:
		return strconv.Itoa(math.MaxUint8)
	case Uint16:
		return strconv.Itoa(math.MaxUint16)
	case Uint32:
		return strconv.FormatUint(math.MaxUint32, 10)
	case Uint64:
		return strconv.FormatUint(math.MaxUint64, 10)
	}
	return "NaN"
}

// parseValue parses s, a value of the schema, as a value of p: an integer
// in p's range, a float, or a char as a single character or its code.
func (p Primitive) parseValue(s string) (string, error) {
	switch {
	case p == Char:
		if len(s) == 1 {
			return strconv.Itoa(int(s[0])), nil
		}
		if n, err := strconv.ParseUint(s, 0, 8); err == nil {
			return strconv.FormatUint(n, 10), nil
		}
		return "", fmt.Errorf("%q is not a char", s)
	case p.Signed():
		n, err := strconv.ParseInt(s, 0, p.Size()*8)
		if err != nil {
			return "", fmt.Errorf("%q is not an %s", s, p)
		}
		return strconv.FormatInt(n, 10), nil
	case p.Unsigned():
		n, err := strconv.ParseUint(s, 0, p.Size()*8)
		if err != nil {
			return "", fmt.Errorf("%q is not a %s", s, p)
		}
		return strconv.FormatUint(n, 10), nil
	}
	if s == "NaN" {
		return s, nil
	}
	f, err := strconv.ParseFloat(s, p.Size()*8)
	if err != nil {
		return "", fmt.Errorf("%q is not a %s", s, p)
	}
	return strconv.FormatFloat(f, 'g', -1, p.Size()*8), nil
}

// Presence tells whether a value is sent, and whether it may be null.
type Presence int

// The presences of §4.3.
const (
	Required Presence = iota
	Optional
	Constant
)

func (p Presence) String() string {
	return [...]string{"required", "optional", "constant"}[p]
}

// Kind tells which of the encodings of §6.5 a Type is.
type Kind int

// The kinds of encoding.
const (
	KindSimple Kind = iota
	KindComposite
	KindEnum
	KindSet
)

// Type is an encoding: a simple type, a composite, an enum or a set.
type Type struct {
	Kind         Kind
	Name         string // its name, global for a type of <types>, local for an element of a composite
	Description  string
	SemanticType string
	SinceVersion int
	Deprecated   int // 0 if not deprecated

	// A simple type.
	Primitive         Primitive
	Length            int // elements, 1 for a scalar, 0 for variable-length data
	Presence          Presence
	Null              string // the null value, as Go source, for an optional scalar
	MinValue          string // as Go source, "" if the schema gives none
	MaxValue          string
	Constant          string // the constant value, as schema text, if Presence is Constant
	ValueRef          string // or the "Enum.Value" it is
	CharacterEncoding string

	// A composite.
	Elements []*Element

	// An enum or a set: the simple type encoding it, and its valid values
	// or choices.
	Encoding *Type
	Values   []*Value

	// Size is the octets the type takes on the wire: 0 for a constant.
	Size int
}

// Scalar reports whether t is a simple type of one element.
func (t *Type) Scalar() bool { return t.Kind == KindSimple && t.Length == 1 }

// Element is a member of a composite: an inline type or a <ref>.
type Element struct {
	Name   string
	Type   *Type
	Ref    bool // a <ref> to a type of <types>
	Offset int  // from the start of the composite
}

// Value is a valid value of an enum or a choice of a set.
type Value struct {
	Name         string
	Description  string
	Value        string // the value as Go source: an integer, or the bit of a choice
	SinceVersion int
	Deprecated   int
}

// Block is what a message and a group entry hold: fixed-length fields,
// then groups, then variable-length data (§5.5).
type Block struct {
	Name         string
	ID           uint64
	Description  string
	SemanticType string
	SinceVersion int
	Deprecated   int
	// BlockLength is the space of the fixed-length fields: the schema's
	// blockLength, or their sizes summed.
	BlockLength int
	Fields      []*Field
	Groups      []*Group
	Data        []*Data
}

// Message is a message template.
type Message struct {
	Block
}

// Group is a repeating group.
type Group struct {
	Block
	Dimension *Type // a composite with blockLength and numInGroup
	// MinEntries and MaxEntries bound the entry count: numInGroup's
	// minValue and maxValue, or 0 and its type's largest value but one.
	// SBE says both that a count goes up to its type's largest value
	// (§5.4.10.1) and that a required integer does not hold its null, that
	// value (§4.4.2), as Real Logic's codecs take it: an encoder sends no
	// more than MaxEntries, a decoder accepts up to the type's largest value
	// unless the schema gives maxValue.
	MinEntries, MaxEntries uint64
	// MaxEntriesSet tells the schema gives numInGroup a maxValue.
	MaxEntriesSet bool
}

// Field is a fixed-length field of a message or group entry.
type Field struct {
	Name         string
	ID           uint64
	Description  string
	SemanticType string
	SinceVersion int
	Deprecated   int
	Type         *Type
	Presence     Presence
	// ValueRef is the constant of a constant field, as "Enum.Value", if
	// given by valueRef.
	ValueRef string
	Offset   int
}

// Data is a variable-length data field.
type Data struct {
	Name         string
	ID           uint64
	Description  string
	SemanticType string
	SinceVersion int
	Deprecated   int
	Type         *Type // a composite of a length and a varData of length 0
	// MaxLength is the longest value: the length's maxValue, or its type's
	// largest value but one, the null value a length may not hold (§4.7.4,
	// §4.8.3).
	MaxLength uint64
	// MinLength is the shortest value: the length's minValue, or 0.
	MinLength uint64
}

// Schema is a loaded and checked message schema.
type Schema struct {
	Package         string
	ID              uint64
	Version         int
	SemanticVersion string
	Description     string
	BigEndian       bool
	// Header is the message header encoding, with blockLength,
	// templateId, schemaId and version elements.
	Header   *Type
	Types    []*Type // the types of <types>, in schema order
	Messages []*Message
}
