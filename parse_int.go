package fix2

import (
	"fmt"
	"math"
	"strconv"
)

// ParseInt parses a FIX integer value from a byte slice.
// FIX integers are sequences of digits with an optional leading '-' sign.
func ParseInt(v []byte) (int, error) {
	if len(v) == 0 {
		return 0, fmt.Errorf("ParseInt: empty input")
	}

	start := 0
	neg := false
	if v[0] == '-' {
		neg = true
		start = 1
		if len(v) == 1 {
			return 0, fmt.Errorf("ParseInt: invalid input %q", v)
		}
	}

	// Accumulate the magnitude as uint so that -MinInt (MaxInt+1) fits.
	limit := uint(math.MaxInt)
	if neg {
		limit++
	}
	checked := len(v)-start > maxSafeDigits
	var n uint
	for i := start; i < len(v); i++ {
		c := v[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("ParseInt: invalid byte '%c' in %q", c, v)
		}
		d := uint(c - '0')
		if checked && n > (limit-d)/10 {
			return 0, fmt.Errorf("ParseInt: value out of range %q: %w", v, strconv.ErrRange)
		}
		n = n*10 + d
	}

	if neg {
		return -int(n), nil // for n == MaxInt+1, int(n) is MinInt and -MinInt == MinInt
	}
	return int(n), nil
}

// maxSafeDigits is the longest run of decimal digits that cannot overflow an
// int: 18 on 64-bit platforms, 9 on 32-bit ones. Longer inputs take the
// overflow-checked path.
const maxSafeDigits = 18 * strconv.IntSize / 64

// ParseFloat parses a FIX float value (FLOAT, PRICE, QTY, AMT, PERCENTAGE)
// from a byte slice. FIX floats are digits with an optional decimal point and
// leading minus sign: exponents, NaN, infinities, hexadecimal and the other
// forms strconv accepts are rejected, as are values beyond float64's range.
func ParseFloat(v []byte) (float64, error) {
	if len(v) == 0 {
		return 0, fmt.Errorf("ParseFloat: empty input")
	}
	if !isFIXFloat(v) {
		return 0, fmt.Errorf("ParseFloat: invalid input %q", v)
	}
	return strconv.ParseFloat(string(v), 64)
}

// isFIXFloat reports whether v is an optional '-' followed by digits holding
// at most one '.', with at least one digit.
func isFIXFloat(v []byte) bool {
	if len(v) > 0 && v[0] == '-' {
		v = v[1:]
	}
	digits, point := false, false
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9':
			digits = true
		case c == '.' && !point:
			point = true
		default:
			return false
		}
	}
	return digits
}

// AppendFloat appends v to dst in FIX float syntax: decimal digits, without
// exponent. It panics if v is NaN or infinite, which FIX cannot express.
func AppendFloat(dst []byte, v float64) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		panic(fmt.Sprintf("fix2: %v is not a FIX float", v))
	}
	return strconv.AppendFloat(dst, v, 'f', -1, 64)
}
